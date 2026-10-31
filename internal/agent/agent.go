// Package agent watches a cluster and pushes normalized objects to the hub.
package agent

import (
	"context"
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
	Resume func(ctx context.Context, cluster, uid string) (map[string]string, error)
	// AuditAddr, if set, is where the agent receives the API server's audit webhook over TLS
	// (AuditCert, AuditKey). The webhook must send AuditToken as a bearer token.
	AuditAddr, AuditToken, AuditCert, AuditKey string

	uid string
}

// Run watches every resource until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	ns, err := a.Client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).
		Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read cluster identity (kube-system UID): %w", err)
	}
	a.uid = string(ns.GetUID())

	var cps map[string]string
	if err := retry(ctx, "resume", func() (err error) {
		cps, err = a.Resume(ctx, a.Cluster, a.uid)
		return err
	}); err != nil {
		return err
	}
	if a.AuditAddr != "" {
		// Listen only now: forwarded audit events need the cluster UID.
		l, err := net.Listen("tcp", a.AuditAddr)
		if err != nil {
			return fmt.Errorf("audit webhook: %w", err)
		}
		go a.serveAudit(ctx, l)
	}
	slog.Info("agent started", "cluster", a.Cluster, "uid", a.uid, "resources", len(a.Resources), "audit", a.AuditAddr)

	var wg sync.WaitGroup
	for _, gvr := range a.Resources {
		wg.Go(func() { a.watch(ctx, gvr, cps[key(gvr)]) })
	}
	wg.Wait()
	return ctx.Err()
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
// the last resourceVersion.
func (a *Agent) consume(ctx context.Context, gvr schema.GroupVersionResource, w watch.Interface, rv string) (string, error) {
	for {
		var ev watch.Event
		select {
		case <-ctx.Done(): // don't rely on the watch closing its channel on cancel
			return rv, ctx.Err()
		case e, ok := <-w.ResultChan():
			if !ok {
				return rv, nil // server closed the watch: resume from rv
			}
			ev = e
		}
		if ev.Type == watch.Error {
			return rv, apierrors.FromObject(ev.Object)
		}
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return rv, fmt.Errorf("unexpected watch object %T", ev.Object)
		}
		b := hub.Batch{Checkpoints: map[string]string{key(gvr): u.GetResourceVersion()}}
		switch {
		case ev.Type == watch.Bookmark:
		case gvr == eventsGVR:
			// An Event's deletion is the API server expiring it: the archive keeps it.
			if ev.Type != watch.Deleted {
				b.Events = []store.Event{toEvent(u)}
			}
		default:
			now := time.Now()
			it, err := toItem(u, false, ev.Type == watch.Deleted, now)
			if err != nil {
				return rv, err
			}
			b.Items = []hub.Item{it}
			if ev.Type != watch.Deleted {
				b.Deploys = toDeploys(gvr, u, now)
			}
		}
		// ponytail: one push per event; batch when the hub becomes the bottleneck.
		if err := a.push(ctx, b); err != nil {
			return rv, err
		}
		rv = u.GetResourceVersion()
	}
}

// relist sends every object as a listed item, then tells the hub the list is complete
// so it can record deletions that happened while nobody was watching.
func (a *Agent) relist(ctx context.Context, gvr schema.GroupVersionResource) (string, error) {
	// ponytail: one unpaged list; switch to a pager for resources with tens of thousands of objects.
	list, err := a.Client.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	rv := list.GetResourceVersion()
	if gvr == eventsGVR {
		// Archived, not mirrored: no Synced, so expired events stay in the hub.
		evs := make([]store.Event, 0, len(list.Items))
		for i := range list.Items {
			evs = append(evs, toEvent(&list.Items[i]))
		}
		for len(evs) > 500 {
			if err := a.push(ctx, hub.Batch{Events: evs[:500]}); err != nil {
				return "", err
			}
			evs = evs[500:]
		}
		return rv, a.push(ctx, hub.Batch{Events: evs, Checkpoints: map[string]string{key(gvr): rv}})
	}
	now := time.Now()
	items := make([]hub.Item, 0, len(list.Items))
	keep := make([]string, 0, len(list.Items))
	var deploys []store.Deploy
	for i := range list.Items {
		it, err := toItem(&list.Items[i], true, false, now)
		if err != nil {
			return "", err
		}
		items = append(items, it)
		deploys = append(deploys, toDeploys(gvr, &list.Items[i], now)...)
		keep = append(keep, it.Namespace+"/"+it.Name)
	}
	for len(items) > 500 {
		if err := a.push(ctx, hub.Batch{Items: items[:500]}); err != nil {
			return "", err
		}
		items = items[500:]
	}
	err = a.push(ctx, hub.Batch{
		Items:       items,
		Deploys:     deploys,
		Synced:      []hub.Synced{{Group: gvr.Group, Kind: strings.TrimSuffix(list.GetKind(), "List"), Keep: keep, Time: now}},
		Checkpoints: map[string]string{key(gvr): rv},
	})
	return rv, err
}

// push retries until the hub accepts the batch: the watch simply waits, and if the
// hub is down long enough for the resume point to expire, the next relist catches up.
func (a *Agent) push(ctx context.Context, b hub.Batch) error {
	b.Cluster, b.ClusterUID = a.Cluster, a.uid
	return retry(ctx, "push", func() error { return a.Push(ctx, b) })
}

func retry(ctx context.Context, what string, f func() error) error {
	for backoff := time.Second; ; backoff = min(2*backoff, time.Minute) {
		err := f()
		if err == nil || ctx.Err() != nil {
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
