package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

// Backfill turns existing API server audit logs into history recorded before mnemo ran.
// It needs the RequestResponse audit level (the written objects), and only works before
// any agent has run for the cluster: history is a chain of versions, and older ones can't
// be inserted under newer ones. Pushes use their own session, so the hub refuses them
// (ErrSuperseded) once an agent has started. The hub also skips what is no newer than
// what it has for the object, so a backfill can be run again safely.
type Backfill struct {
	Cluster, ClusterUID string
	Resources           []schema.GroupVersionResource
	Push                func(context.Context, hub.Batch) error
	BatchSize           int // default 500

	writes  []write
	skipped int // deletes Run couldn't place
}

// write is one successful write found in the logs.
type write struct {
	at       time.Time
	verb     string
	resource string // group/resource, as the request named it
	rv       uint64 // the etcd revision it produced (0: unknown, e.g. a delete answered with a Status)
	item     hub.Item
	audit    hub.Audit
	delete   bool
}

// Read collects the writes to watched resources in r (audit.k8s.io/v1 Events, one JSON
// object after another, as the API server's log backend writes them), and reports how many
// it found. Logs from several API servers can all be read: Run orders them.
// ponytail: every write is held in memory until Run; stream per object if logs outgrow RAM.
func (b *Backfill) Read(r io.Reader) (int, error) {
	watch := &Agent{Resources: b.Resources}
	versions := map[string]string{} // group/resource -> the watched version
	for _, gvr := range b.Resources {
		versions[gvr.Group+"/"+gvr.Resource] = gvr.Version
	}
	d := json.NewDecoder(r)
	n := 0
	for {
		var raw json.RawMessage
		if err := d.Decode(&raw); err == io.EOF {
			return n, nil
		} else if errors.Is(err, io.ErrUnexpectedEOF) {
			slog.Warn("audit log ends mid-record (a log being written, or cut by rotation); keeping what came before", "writes", n)
			return n, nil
		} else if err != nil {
			return n, fmt.Errorf("at byte %d: %w", d.InputOffset(), err)
		}
		var e auditEvent
		var more struct {
			RequestURI     string          `json:"requestURI"`
			StageTimestamp time.Time       `json:"stageTimestamp"`
			ResponseObject json.RawMessage `json:"responseObject"`
		}
		if json.Unmarshal(raw, &e) != nil || json.Unmarshal(raw, &more) != nil {
			continue // not an audit event
		}
		audits := watch.audits([]auditEvent{e})
		// Subresources (scale, status, ...) answer with another kind of object; dry runs
		// change nothing; Events are archived on their own by the agent, not versioned.
		if len(audits) == 0 || e.ObjectRef.Subresource != "" || strings.Contains(more.RequestURI, "dryRun=") ||
			(e.ObjectRef.APIGroup == "" && e.ObjectRef.Resource == eventsGVR.Resource) {
			continue
		}
		res := e.ObjectRef.APIGroup + "/" + e.ObjectRef.Resource
		var u unstructured.Unstructured
		if err := u.UnmarshalJSON(more.ResponseObject); err != nil && e.Verb != "delete" {
			continue
		}
		w := write{at: more.StageTimestamp, verb: e.Verb, resource: res, audit: audits[0]}
		isObject := u.GetKind() != "" && u.GetKind() != "Status"
		gone := isObject && u.GetDeletionTimestamp() != nil && len(u.GetFinalizers()) == 0
		switch {
		case isObject && !gone && (e.Verb != "delete" || len(u.GetFinalizers()) > 0): // a delete held by finalizers only marks the object
			if gv := schema.FromAPIVersionAndKind(u.GetAPIVersion(), "").GroupVersion(); gv.Version != versions[res] {
				continue // another version of the object than the agent records: it would diff on every field
			}
			it, err := toItem(&u, false, false, w.at)
			if err != nil {
				continue
			}
			w.item = it
			w.rv, _ = strconv.ParseUint(u.GetResourceVersion(), 10, 64)
		case e.Verb == "delete" || gone: // gone: the update that removed the last finalizer
			w.delete = true
			uid := audits[0].UID
			if isObject {
				uid = string(u.GetUID())
			}
			// Attribute the deletion row only (by UID), never the object's last version.
			w.audit.UID, w.audit.ResourceVersion = uid, ""
			kind := "" // set in order, unless the response says
			if isObject {
				kind = u.GetKind()
			}
			w.item.Change = store.Change{Ref: store.Ref{Group: e.ObjectRef.APIGroup, Kind: kind,
				Namespace: e.ObjectRef.Namespace, Name: e.ObjectRef.Name}, UID: uid, Time: w.at}
		default:
			continue
		}
		b.writes = append(b.writes, w)
		n++
	}
}

// Run pushes what Read found to the hub, and reports how many writes it sent.
func (b *Backfill) Run(ctx context.Context) (int, error) {
	writes := b.order()
	var items []hub.Item
	var audits []hub.Audit
	for _, w := range writes {
		items = append(items, w.item)
		audits = append(audits, w.audit)
	}
	if b.skipped > 0 {
		slog.Warn("some deletes couldn't be placed (no UID, or a kind never written in these logs); the agent's first list records them", "deletes", b.skipped)
	}

	size := b.BatchSize
	if size <= 0 {
		size = 500
	}
	session := "backfill-" + rand.Text()
	for seq, i := int64(1), 0; i < len(items); seq, i = seq+1, i+size {
		j := min(i+size, len(items))
		batch := hub.Batch{Cluster: b.Cluster, ClusterUID: b.ClusterUID, Session: session, Stream: hub.BackfillStream, Seq: seq,
			Items: items[i:j], Audits: audits[i:j]}
		var err error
		for attempt := range 5 {
			var he *hub.HTTPError
			if err = b.Push(ctx, batch); err == nil || errors.Is(err, store.ErrSuperseded) || ctx.Err() != nil ||
				(errors.As(err, &he) && he.Code/100 == 4 && he.Code != 429) { // a bad token or cluster UID won't fix itself
				break
			}
			slog.Warn("push failed; retrying", "err", err, "attempt", attempt+1)
			sleepCtx(ctx, time.Duration(attempt+1)*2*time.Second)
		}
		if errors.Is(err, store.ErrSuperseded) {
			return i, errors.New("an agent has run for this cluster: backfill only works before the first agent run")
		}
		if err != nil {
			return i, fmt.Errorf("push: %w", err)
		}
	}
	return len(items), nil
}

// order sorts the writes by time, then each object's writes by the revision they produced
// (API servers' clocks differ, and the log order isn't the commit order), and drops what
// would record wrongly:
//   - a delete of a kind never written in the logs, or without a UID;
//   - a write that changed nothing (same revision as the one before): its audit would
//     credit the previous write's author;
//   - nothing else, but an object's first write that isn't a create becomes a baseline:
//     the object existed before the logs start.
func (b *Backfill) order() []write {
	b.skipped = 0
	writes := slices.Clone(b.writes)
	slices.SortStableFunc(writes, func(x, y write) int { return x.at.Compare(y.at) })
	kinds := map[string]string{} // a delete's response rarely carries the object: take the kind from other writes
	maxRV := map[string]uint64{} // by UID
	for _, w := range writes {
		if !w.delete {
			kinds[w.resource] = w.item.Kind
			maxRV[w.item.UID] = max(maxRV[w.item.UID], w.rv)
		}
	}
	var kept []write
	positions := map[string][]int{} // object -> its indexes in kept
	deleted := map[string]bool{}    // by UID: a graceful delete, then the final one, credit the first
	for _, w := range writes {
		if w.delete {
			if deleted[w.item.UID] {
				continue
			}
			deleted[w.item.UID] = true
			if w.item.Kind == "" {
				w.item.Kind = kinds[w.resource]
			}
			if w.item.Kind == "" || w.item.UID == "" {
				b.skipped++
				continue
			}
			w.rv = maxRV[w.item.UID] // after every write of that object
		}
		k := w.resource + "/" + w.item.Namespace + "/" + w.item.Name
		positions[k] = append(positions[k], len(kept))
		kept = append(kept, w)
	}
	// Per object: by revision (a delete right after its object's last write); keep each
	// object's slots in the time order, so objects still interleave by time.
	for _, idx := range positions {
		ws := make([]write, len(idx))
		for i, j := range idx {
			ws[i] = kept[j]
		}
		if !slices.ContainsFunc(ws, func(w write) bool { return w.rv == 0 && !w.delete }) { // unknown revisions: trust time
			slices.SortStableFunc(ws, func(x, y write) int {
				if x.rv != y.rv {
					return cmpU(x.rv, y.rv)
				}
				return boolInt(x.delete) - boolInt(y.delete)
			})
		}
		var last time.Time
		var lastRV uint64
		for i, j := range idx {
			w := ws[i]
			if !w.delete && i > 0 && !ws[i-1].delete && w.rv != 0 && w.rv == lastRV {
				w.item.Kind = "" // a no-op write: dropped below
			}
			lastRV = w.rv
			if i == 0 && !w.delete && w.verb != "create" {
				w.item.Listed = true // existed before the logs: a baseline, not a creation
			}
			if !w.at.After(last) { // times strictly increase per object, so the hub can skip what it has
				w.at = last.Add(time.Microsecond)
				w.item.Time = w.at
			}
			last = w.at
			kept[j] = w
		}
	}
	return slices.DeleteFunc(kept, func(w write) bool { return w.item.Kind == "" })
}

func cmpU(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
