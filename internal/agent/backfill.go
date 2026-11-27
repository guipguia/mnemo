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
// (ErrSuperseded) once an agent has started, and skips versions it already has, so a
// backfill can be run again after a crash, or with logs that add later writes (not earlier
// ones: those would be appended after the newer versions).
type Backfill struct {
	Cluster, ClusterUID string
	Resources           []schema.GroupVersionResource
	Push                func(context.Context, hub.Batch) error
	BatchSize           int // default 500

	writes []write
}

// write is one successful write found in the logs.
type write struct {
	at       time.Time
	resource string // group/resource, as the request named it
	item     hub.Item
	audit    hub.Audit
	delete   bool
}

// Read collects the writes to watched resources in r (audit.k8s.io/v1 Events, one JSON
// object after another, as the API server's log backend writes them), and reports how many
// it found. Logs from several API servers can all be read: Run sorts them.
// ponytail: every write is held in memory until Run; stream per object if logs outgrow RAM.
func (b *Backfill) Read(r io.Reader) (int, error) {
	watch := &Agent{Resources: b.Resources}
	d := json.NewDecoder(r)
	n := 0
	for {
		var raw json.RawMessage
		if err := d.Decode(&raw); err == io.EOF {
			return n, nil
		} else if err != nil {
			return n, err
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
		// change nothing.
		if len(audits) == 0 || e.ObjectRef.Subresource != "" || strings.Contains(more.RequestURI, "dryRun=") {
			continue
		}
		var u unstructured.Unstructured
		if err := u.UnmarshalJSON(more.ResponseObject); err != nil && e.Verb != "delete" {
			continue
		}
		w := write{at: more.StageTimestamp, resource: e.ObjectRef.APIGroup + "/" + e.ObjectRef.Resource, audit: audits[0]}
		switch {
		case u.GetKind() != "" && u.GetKind() != "Status" &&
			(e.Verb != "delete" || len(u.GetFinalizers()) > 0): // a delete held by finalizers only marks the object
			it, err := toItem(&u, false, false, w.at)
			if err != nil {
				continue
			}
			w.item = it
		case e.Verb == "delete":
			w.delete = true
			w.item.Change = store.Change{Ref: store.Ref{Group: e.ObjectRef.APIGroup, // Kind: set in Run
				Namespace: e.ObjectRef.Namespace, Name: e.ObjectRef.Name}, UID: audits[0].UID, Time: w.at}
		default:
			continue
		}
		b.writes = append(b.writes, w)
		n++
	}
}

// Run pushes what Read found to the hub, oldest first, and reports how many writes it sent.
func (b *Backfill) Run(ctx context.Context) (int, error) {
	writes := b.writes
	slices.SortStableFunc(writes, func(x, y write) int { return x.at.Compare(y.at) })
	// A delete's response rarely carries the object: take the kind from the resource's other writes.
	kinds := map[string]string{}
	for _, w := range writes {
		if !w.delete {
			kinds[w.resource] = w.item.Kind
		}
	}
	var items []hub.Item
	var audits []hub.Audit
	for _, w := range writes {
		if w.delete {
			kind, ok := kinds[w.resource]
			if !ok {
				continue // never written in these logs: nothing recorded to delete
			}
			w.item.Kind = kind
		}
		items = append(items, w.item)
		audits = append(audits, w.audit)
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
			if err = b.Push(ctx, batch); err == nil || errors.Is(err, store.ErrSuperseded) || ctx.Err() != nil {
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

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
