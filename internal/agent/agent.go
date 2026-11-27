// Package agent watches a cluster and pushes normalized objects to the hub.
package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

// DefaultResources is what an agent watches unless told otherwise.
var DefaultResources = []string{
	"apps/v1/deployments", "apps/v1/statefulsets", "apps/v1/daemonsets",
	"v1/configmaps", "v1/services", "v1/serviceaccounts",
	"networking.k8s.io/v1/ingresses",
	"rbac.authorization.k8s.io/v1/roles", "rbac.authorization.k8s.io/v1/rolebindings",
	"rbac.authorization.k8s.io/v1/clusterroles", "rbac.authorization.k8s.io/v1/clusterrolebindings",
	"v1/events",
}

// ParseResource parses "group/version/resource" or "version/resource" (core group).
func ParseResource(s string) (schema.GroupVersionResource, error) {
	var gvr schema.GroupVersionResource
	switch p := strings.Split(s, "/"); len(p) {
	case 2:
		gvr = schema.GroupVersionResource{Version: p[0], Resource: p[1]}
	case 3:
		gvr = schema.GroupVersionResource{Group: p[0], Version: p[1], Resource: p[2]}
	default:
		return gvr, fmt.Errorf("resource %q: want group/version/resource or version/resource", s)
	}
	if gvr.Group == "" && gvr.Resource == "secrets" {
		return gvr, errors.New("secrets are never recorded: mnemo would store their values")
	}
	return gvr, nil
}

func key(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" {
		return gvr.Version + "/" + gvr.Resource
	}
	return gvr.Group + "/" + gvr.Version + "/" + gvr.Resource
}

type Agent struct {
	Client    dynamic.Interface
	Cluster   string
	Resources []schema.GroupVersionResource
	// Push and Resume reach the hub: over HTTP (hub.Client) or in-process (mnemo all).
	Push   func(context.Context, hub.Batch) error
	Resume func(ctx context.Context, cluster, uid, session string) (map[string]string, error)
	// AuditAddr, if set, is where the agent receives the API server's audit webhook over TLS
	// (AuditCert, AuditKey). The webhook must send AuditToken as a bearer token.
	AuditAddr, AuditToken, AuditCert, AuditKey string

	uid     string
	session string // this run: the hub drops batches from earlier runs
	stop    context.CancelCauseFunc

	mu  sync.Mutex
	seq map[string]int64 // last batch number per stream (watched resource)
}

// Run watches every resource until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	ns, err := a.Client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).
		Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read cluster identity (kube-system UID): %w", err)
	}
	a.uid = string(ns.GetUID())
	a.session = rand.Text()
	ctx, a.stop = context.WithCancelCause(ctx)
	defer a.stop(nil)

	// Listen before taking over the cluster at Resume: a pod that can't serve shouldn't
	// replace a working agent.
	var audit net.Listener
	if a.AuditAddr != "" {
		if audit, err = net.Listen("tcp", a.AuditAddr); err != nil {
			return fmt.Errorf("audit webhook: %w", err)
		}
	}

	var cps map[string]string
	if err := retry(ctx, "resume", func() (err error) {
		cps, err = a.Resume(ctx, a.Cluster, a.uid, a.session)
		return err
	}); err != nil {
		return err
	}
	if audit != nil {
		go a.serveAudit(ctx, audit) // only now: forwarded audit events need the session's UID
	}
	slog.Info("agent started", "cluster", a.Cluster, "uid", a.uid, "resources", len(a.Resources), "audit", a.AuditAddr)

	var wg sync.WaitGroup
	for _, gvr := range a.Resources {
		wg.Go(func() { a.watch(ctx, gvr, cps[key(gvr)]) })
	}
	wg.Go(func() { a.heartbeat(ctx) })
	wg.Wait()
	return context.Cause(ctx)
}

// watch resumes from rv, relisting whenever there is no usable resume point.
func (a *Agent) watch(ctx context.Context, gvr schema.GroupVersionResource, rv string) {
	log := slog.With("resource", key(gvr))
	for ctx.Err() == nil {
		var err error
		if rv == "" {
			if rv, err = a.relist(ctx, gvr); err != nil {
				log.Error("relist failed", "err", err)
				sleep(ctx, 5*time.Second)
				continue
			}
		}
		w, err := a.Client.Resource(gvr).Watch(ctx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true})
		if err == nil {
			rv, err = a.consume(ctx, gvr, w, rv)
			w.Stop()
		}
		switch {
		case apierrors.IsResourceExpired(err) || apierrors.IsGone(err):
			// Offline past etcd's compaction window: relist and diff against the hub.
			log.Info("resume point expired, relisting")
			rv = ""
		case err != nil && ctx.Err() == nil:
			log.Error("watch failed", "err", err)
			sleep(ctx, 5*time.Second)
		}
	}
}

// consume pushes watch events until the server closes the watch or ctx is done, returning
// the resourceVersion of the last event the hub has. Events go in batches, up to watchBatch
// objects or watchLinger after the first one: one hub transaction for a burst, not one per
// change. Pending events are pushed before the watch ends or fails.
func (a *Agent) consume(ctx context.Context, gvr schema.GroupVersionResource, w watch.Interface, rv string) (string, error) {
	var b hub.Batch
	var pending string // the resourceVersion b reaches; "" when there's nothing to push
	var linger <-chan time.Time
	size := 0 // object bytes in b
	flush := func() error {
		if pending == "" {
			return nil
		}
		b.Checkpoints = map[string]string{key(gvr): pending}
		if err := a.push(ctx, key(gvr), b); err != nil {
			return err
		}
		rv, b, pending, linger, size = pending, hub.Batch{}, "", nil, 0
		return nil
	}
	for {
		var ev watch.Event
		select {
		case <-ctx.Done(): // don't rely on the watch closing its channel on cancel
			// Push what's pending, briefly: redelivered after a restart, those events would
			// be stamped with the restart's time instead of when they happened.
			stop := ctx
			ctx, cancel := context.WithTimeout(context.WithoutCancel(stop), 2*time.Second)
			defer cancel()
			if pending != "" {
				b.Checkpoints = map[string]string{key(gvr): pending}
				if a.push(ctx, key(gvr), b) == nil {
					rv = pending
				}
			}
			return rv, stop.Err()
		case <-linger:
			if err := flush(); err != nil {
				return rv, err
			}
			continue
		case e, ok := <-w.ResultChan():
			if !ok {
				err := flush() // server closed the watch: resume from rv
				return rv, err
			}
			ev = e
		}
		if ev.Type == watch.Error {
			if err := flush(); err != nil {
				return rv, err
			}
			return rv, apierrors.FromObject(ev.Object)
		}
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			if err := flush(); err != nil {
				return rv, err
			}
			return rv, fmt.Errorf("unexpected watch object %T", ev.Object)
		}
		switch {
		case ev.Type == watch.Bookmark:
		case gvr == eventsGVR:
			// An Event's deletion is the API server expiring it: the archive keeps it.
			if ev.Type != watch.Deleted {
				b.Events = append(b.Events, toEvent(u))
			}
		default:
			now := time.Now()
			it, err := toItem(u, false, ev.Type == watch.Deleted, now)
			if err != nil {
				return rv, err
			}
			b.Items = append(b.Items, it)
			size += len(it.Object)
			if ev.Type != watch.Deleted {
				b.Deploys = append(b.Deploys, toDeploys(gvr, u, now)...)
			}
		}
		pending = u.GetResourceVersion()
		if linger == nil {
			linger = time.After(watchLinger)
		}
		if len(b.Items)+len(b.Events) >= watchBatch || size >= watchBatchBytes {
			if err := flush(); err != nil {
				return rv, err
			}
		}
	}
}

const (
	watchBatch      = 100
	watchBatchBytes = 8 << 20 // well under the hub's 64 MiB request limit, even with managedFields
	watchLinger     = 200 * time.Millisecond
)

// relist sends every object as a listed item, then tells the hub the list is complete
// so it can record deletions that happened while nobody was watching.
//
// It reads in pages, pushed as they come: memory stays at a page, plus a name per object
// for the Synced list. Pages all come from the first one's snapshot, which expires with
// etcd compaction (5 minutes by default); pushing at the hub's pace, a large resource can
// outlast it. Then it reads every page first, holding them converted (a few KB per object,
// not the API objects), and pushes them after.
func (a *Agent) relist(ctx context.Context, gvr schema.GroupVersionResource) (string, error) {
	rv, err := a.relistPages(ctx, gvr, false)
	if errors.Is(err, errSnapshotExpired) {
		slog.Warn("relist outlasted its snapshot; reading every page before pushing", "resource", key(gvr))
		rv, err = a.relistPages(ctx, gvr, true)
	}
	return rv, err
}

var errSnapshotExpired = errors.New("list snapshot expired between pages")

func (a *Agent) relistPages(ctx context.Context, gvr schema.GroupVersionResource, hold bool) (string, error) {
	opts := metav1.ListOptions{Limit: listPage}
	var rv, kind string
	var keep []string
	var held []hub.Batch
	now := time.Now() // one time for the whole relist: when it started
	for {
		list, err := a.Client.Resource(gvr).List(ctx, opts)
		if err != nil {
			if opts.Continue != "" && (apierrors.IsResourceExpired(err) || apierrors.IsGone(err)) {
				return "", errSnapshotExpired
			}
			return "", err
		}
		if rv == "" {
			rv, kind = list.GetResourceVersion(), strings.TrimSuffix(list.GetKind(), "List")
		}
		var b hub.Batch
		for i := range list.Items {
			if gvr == eventsGVR {
				b.Events = append(b.Events, toEvent(&list.Items[i]))
				continue
			}
			it, err := toItem(&list.Items[i], true, false, now)
			if err != nil {
				return "", err
			}
			b.Items = append(b.Items, it)
			b.Deploys = append(b.Deploys, toDeploys(gvr, &list.Items[i], now)...) // upserts: fine per page
			keep = append(keep, it.Namespace+"/"+it.Name)
		}
		opts.Continue = list.GetContinue()
		last := opts.Continue == ""
		if last { // the last page also says the relist is complete
			b.Checkpoints = map[string]string{key(gvr): rv}
			if gvr != eventsGVR { // Events are archived, not mirrored: no Synced, so expired ones stay in the hub
				b.Synced = []hub.Synced{{Group: gvr.Group, Kind: kind, Keep: keep, Time: now}}
			}
		}
		if hold {
			held = append(held, b)
		} else {
			held = []hub.Batch{b}
		}
		if hold && !last {
			continue
		}
		for _, hb := range held {
			if len(hb.Items)+len(hb.Events) == 0 && hb.Checkpoints == nil {
				continue // a page can come back empty (filtered); the last one always goes
			}
			if err := a.push(ctx, key(gvr), hb); err != nil {
				return "", err
			}
		}
		if last {
			return rv, nil
		}
	}
}

// listPage is how many objects a relist reads, converts and pushes at a time.
const listPage = 500

// heartbeat pushes an empty batch every minute, so the hub's last-seen time (and the
// mnemo_cluster_last_seen_timestamp_seconds metric) tells a quiet cluster from a dead agent.
func (a *Agent) heartbeat(ctx context.Context) {
	for {
		sleep(ctx, heartbeatEvery)
		if ctx.Err() != nil {
			return
		}
		if err := a.Push(ctx, hub.Batch{Cluster: a.Cluster, ClusterUID: a.uid}); err != nil && ctx.Err() == nil {
			slog.Warn("heartbeat failed", "err", err) // no retry: the next one comes in a minute
		}
	}
}

var heartbeatEvery = time.Minute

// push retries until the hub accepts the batch: the watch simply waits, and if the
// hub is down long enough for the resume point to expire, the next relist catches up.
// The batch is numbered in stream once, before any retry, so the hub can drop a stale copy.
func (a *Agent) push(ctx context.Context, stream string, b hub.Batch) error {
	a.mu.Lock()
	if a.seq == nil {
		a.seq = map[string]int64{}
	}
	a.seq[stream]++
	b.Seq = a.seq[stream]
	a.mu.Unlock()
	b.Cluster, b.ClusterUID, b.Session, b.Stream = a.Cluster, a.uid, a.session, stream
	err := retry(ctx, "push", func() error { return a.Push(ctx, b) })
	if errors.Is(err, store.ErrSuperseded) && a.stop != nil {
		a.stop(err) // every watch ends, and Run returns err
	}
	return err
}

func retry(ctx context.Context, what string, f func() error) error {
	for backoff := time.Second; ; backoff = min(2*backoff, time.Minute) {
		err := f()
		if err == nil || ctx.Err() != nil || errors.Is(err, store.ErrSuperseded) { // retrying can't fix that
			return err
		}
		slog.Warn(what+" failed, retrying", "err", err, "in", backoff)
		sleep(ctx, backoff)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
