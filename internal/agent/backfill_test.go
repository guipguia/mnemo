package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

// line is one audit.k8s.io/v1 Event for ConfigMap shop/cfg, as the log backend writes it.
func line(ts, verb, user, sub, uri, resp string) string {
	return fmt.Sprintf(`{"kind":"Event","apiVersion":"audit.k8s.io/v1","level":"RequestResponse","stage":"ResponseComplete",
"requestURI":%q,"verb":%q,"user":{"username":%q},"userAgent":"kubectl/v1.33.0",
"objectRef":{"resource":"configmaps","namespace":"shop","name":"cfg","apiVersion":"v1","subresource":%q},
"responseStatus":{"code":200},"responseObject":%s,"stageTimestamp":"2026-01-01T%sZ"}`, uri, verb, user, sub, resp, ts) + "\n"
}

// cmJSON is the ConfigMap shop/cfg with uid, resourceVersion and a value; extra is more metadata.
func cmJSON(uid, rv, value, extra string) string {
	return fmt.Sprintf(`{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"cfg","namespace":"shop","uid":%q,"resourceVersion":%q%s},"data":{"k":%q}}`,
		uid, rv, extra, value)
}

const (
	uri       = "/api/v1/namespaces/shop/configmaps/cfg"
	statusFor = `{"kind":"Status","apiVersion":"v1","status":"Success","details":{"name":"cfg","kind":"configmaps","uid":%q}}`
)

var cfgRef = store.Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "cfg"}

func newBackfill(t *testing.T) (*store.Store, func(logs ...string) (int, error)) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	run := func(logs ...string) (int, error) {
		b := &Backfill{Cluster: "prod", ClusterUID: "uid-1", BatchSize: 2,
			Push:      func(ctx context.Context, b hub.Batch) error { return hub.Ingest(ctx, st, b) },
			Resources: []schema.GroupVersionResource{{Version: "v1", Resource: "configmaps"}, eventsGVR}}
		for _, l := range logs {
			if _, err := b.Read(strings.NewReader(l)); err != nil {
				return 0, err
			}
		}
		return b.Run(context.Background())
	}
	return st, run
}

func history(t *testing.T, st *store.Store) string {
	t.Helper()
	hist, err := st.History(context.Background(), cfgRef, 20)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := len(hist) - 1; i >= 0; i-- { // oldest first
		c := hist[i]
		got = append(got, fmt.Sprintf("%s %s %s", c.Type, c.Author, c.ResourceVersion))
	}
	return strings.Join(got, ", ")
}

func TestBackfill(t *testing.T) {
	for _, tc := range []struct {
		name string
		logs []string
		want string
	}{{
		name: "two API servers, interleaved; dry runs, subresources and reads skipped",
		logs: []string{
			line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", "")) +
				line("10:02:00", "patch", "bob", "", uri+"?dryRun=All", cmJSON("u1", "2", "dry", "")) +
				line("10:03:00", "delete", "carol", "", uri, fmt.Sprintf(statusFor, "u1")),
			line("10:01:00", "patch", "bob", "", uri, cmJSON("u1", "3", "b", "")) +
				line("10:01:30", "update", "system:serviceaccount:x:y", "status", uri+"/status", cmJSON("u1", "4", "b", "")) +
				`{"kind":"Event","stage":"ResponseComplete","verb":"get","objectRef":{"resource":"configmaps"}}` + "\n",
		},
		want: "added alice 1, modified bob 3, deleted carol ",
	}, {
		name: "a delete answered with the object credits only the deletion",
		logs: []string{line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", "")) +
			line("10:01:00", "patch", "bob", "", uri, cmJSON("u1", "2", "b", "")) +
			line("10:02:00", "delete", "carol", "", uri, cmJSON("u1", "2", "b", ""))},
		want: "added alice 1, modified bob 2, deleted carol ",
	}, {
		name: "a write that changed nothing doesn't take the author",
		logs: []string{line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", "")) +
			line("10:01:00", "patch", "bob", "", uri, cmJSON("u1", "2", "b", "")) +
			line("10:02:00", "patch", "system:serviceaccount:argocd:argocd-application-controller", "", uri, cmJSON("u1", "2", "b", ""))},
		want: "added alice 1, modified bob 2",
	}, {
		name: "an object from before the logs starts as a baseline",
		logs: []string{line("10:00:00", "patch", "alice", "", uri, cmJSON("u1", "7", "a", "")) +
			line("10:01:00", "patch", "bob", "", uri, cmJSON("u1", "8", "b", ""))},
		want: "baseline alice 7, modified bob 8",
	}, {
		name: "a delete held by a finalizer completes when the finalizer goes",
		logs: []string{line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", `,"finalizers":["x"]`)) +
			line("10:01:00", "delete", "bob", "", uri, cmJSON("u1", "2", "a", `,"finalizers":["x"],"deletionTimestamp":"2026-01-01T10:01:00Z"`)) +
			line("10:02:00", "update", "system:serviceaccount:x:ctrl", "", uri, cmJSON("u1", "3", "a", `,"deletionTimestamp":"2026-01-01T10:01:00Z"`)) +
			line("10:03:00", "create", "carol", "", uri, cmJSON("u2", "4", "c", ""))},
		want: "added alice 1, modified bob 2, deleted system:serviceaccount:x:ctrl , added carol 4",
	}, {
		name: "a graceful delete, then the final one: the first deleter is credited",
		logs: []string{line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", "")) +
			line("10:01:00", "delete", "carol", "", uri, cmJSON("u1", "2", "a", `,"deletionTimestamp":"2026-01-01T10:01:00Z"`)) +
			line("10:01:30", "delete", "system:node:n1", "", uri, fmt.Sprintf(statusFor, "u1"))},
		want: "added alice 1, deleted carol ",
	}, {
		name: "revisions order an object's writes, whatever the clocks say",
		logs: []string{
			line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", "")) +
				line("10:01:00.500000", "patch", "bob", "", uri, cmJSON("u1", "2", "b", "")),
			line("10:01:00.000000", "patch", "carol", "", uri, cmJSON("u1", "3", "c", "")), // a clock behind
		},
		want: "added alice 1, modified bob 2, modified carol 3",
	}, {
		name: "events and other API versions are skipped",
		logs: []string{line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", "")) +
			strings.ReplaceAll(line("10:01:00", "create", "bob", "", "/api/v1/namespaces/shop/events", `{"kind":"Event","apiVersion":"v1","metadata":{"name":"e","namespace":"shop","uid":"e1","resourceVersion":"2"}}`),
				`"resource":"configmaps"`, `"resource":"events"`) +
			strings.ReplaceAll(line("10:02:00", "patch", "bob", "", uri, cmJSON("u1", "3", "b", "")), `"apiVersion":"v1","metadata"`, `"apiVersion":"v2","metadata"`)},
		want: "added alice 1",
	}, {
		name: "a record cut off at the end of a log",
		logs: []string{line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", "")) + `{"kind":"Event","stage":"Resp`},
		want: "added alice 1",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			st, run := newBackfill(t)
			if _, err := run(tc.logs...); err != nil {
				t.Fatal(err)
			}
			if got := history(t, st); got != tc.want {
				t.Errorf("history:\n  %s\nwant:\n  %s", got, tc.want)
			}
			// Running it again (after a crash, or by mistake) changes nothing.
			if _, err := run(tc.logs...); err != nil {
				t.Fatal(err)
			}
			if got := history(t, st); got != tc.want {
				t.Errorf("after a second run:\n  %s\nwant:\n  %s", got, tc.want)
			}
		})
	}
}

func TestBackfillRerunRecreated(t *testing.T) {
	st, run := newBackfill(t)
	log := line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", "")) +
		line("10:01:00", "delete", "bob", "", uri, fmt.Sprintf(statusFor, "u1")) +
		line("10:02:00", "create", "carol", "", uri, cmJSON("u2", "5", "c", ""))
	for range 2 {
		if _, err := run(log); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := history(t, st), "added alice 1, deleted bob , added carol 5"; got != want {
		t.Errorf("history %s, want %s", got, want)
	}
	// Earlier logs found later are skipped, not put after the newer versions.
	if _, err := run(line("09:00:00", "create", "zed", "", uri, cmJSON("u0", "0", "z", ""))); err != nil {
		t.Fatal(err)
	}
	if got, want := history(t, st), "added alice 1, deleted bob , added carol 5"; got != want {
		t.Errorf("after earlier logs: %s, want %s", got, want)
	}
}

func TestBackfillRefused(t *testing.T) {
	st, run := newBackfill(t)
	if _, err := hub.Resume(context.Background(), st, "prod", "uid-1", "agent-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", ""))); err == nil || !strings.Contains(err.Error(), "before the first agent run") {
		t.Errorf("backfill after an agent ran: %v, want refused", err)
	}

	pushes := 0
	b := &Backfill{Cluster: "prod", ClusterUID: "other", Resources: []schema.GroupVersionResource{{Version: "v1", Resource: "configmaps"}},
		Push: func(context.Context, hub.Batch) error { pushes++; return &hub.HTTPError{Code: 409} }}
	b.Read(strings.NewReader(line("10:00:00", "create", "alice", "", uri, cmJSON("u1", "1", "a", ""))))
	var he *hub.HTTPError
	if _, err := b.Run(context.Background()); !errors.As(err, &he) || pushes != 1 {
		t.Errorf("409: err %v after %d pushes, want it returned without retrying", err, pushes)
	}
}
