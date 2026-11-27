package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

// auditLine is one audit.k8s.io/v1 Event as the log backend writes it.
func auditLine(ts, verb, user, sub, uri, resp string) string {
	return fmt.Sprintf(`{"kind":"Event","apiVersion":"audit.k8s.io/v1","level":"RequestResponse","stage":"ResponseComplete",
"requestURI":%q,"verb":%q,"user":{"username":%q},"userAgent":"kubectl/v1.33.0",
"objectRef":{"resource":"configmaps","namespace":"shop","name":"cfg","apiVersion":"v1","subresource":%q},
"responseStatus":{"code":200},"responseObject":%s,"stageTimestamp":%q}`, uri, verb, user, sub, resp, ts) + "\n"
}

func cmJSON(rv, value string) string {
	return fmt.Sprintf(`{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"cfg","namespace":"shop","uid":"u1","resourceVersion":%q},"data":{"k":%q}}`, rv, value)
}

func TestBackfill(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	push := func(ctx context.Context, b hub.Batch) error { return hub.Ingest(ctx, st, b) }
	b := &Backfill{Cluster: "prod", ClusterUID: "uid-1", Push: push, BatchSize: 2,
		Resources: []schema.GroupVersionResource{{Version: "v1", Resource: "configmaps"}}}

	// Two API servers' logs, each in order, interleaved in time.
	api1 := auditLine("2026-01-01T10:00:00.000000Z", "create", "alice", "", "/api/v1/namespaces/shop/configmaps", cmJSON("1", "a")) +
		auditLine("2026-01-01T10:02:00.000000Z", "patch", "bob", "", "/api/v1/namespaces/shop/configmaps/cfg?dryRun=All", cmJSON("2", "dry")) +
		auditLine("2026-01-01T10:03:00.000000Z", "delete", "carol", "", "/api/v1/namespaces/shop/configmaps/cfg",
			`{"kind":"Status","apiVersion":"v1","status":"Success","details":{"name":"cfg","kind":"configmaps","uid":"u1"}}`)
	api2 := auditLine("2026-01-01T10:01:00.000000Z", "patch", "bob", "", "/api/v1/namespaces/shop/configmaps/cfg", cmJSON("3", "b")) +
		auditLine("2026-01-01T10:01:30.000000Z", "update", "system:serviceaccount:x:y", "status", "/api/v1/namespaces/shop/configmaps/cfg/status", cmJSON("4", "b")) +
		`{"kind":"Event","stage":"ResponseComplete","verb":"get","objectRef":{"resource":"configmaps"}}` + "\n"
	for _, log := range []string{api1, api2} {
		if _, err := b.Read(strings.NewReader(log)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := b.Run(ctx)
	if err != nil || n != 3 {
		t.Fatalf("Run = %d, %v; want 3 writes", n, err)
	}
	hist, err := st.History(ctx, store.Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "cfg"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range hist {
		got = append(got, fmt.Sprintf("%s %s %s %s", c.Time.Format("15:04"), c.Type, c.Author, c.Source))
	}
	want := []string{"10:03 deleted carol manual", "10:01 modified bob manual", "10:00 added alice manual"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("history:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Run again (as after a crash): the batches are already stored and are dropped.
	if _, err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if hist2, _ := st.History(ctx, store.Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "cfg"}, 10); len(hist2) != 3 {
		t.Errorf("after a second run: %d versions, want 3", len(hist2))
	}

	// Once an agent has run, backfill is refused.
	if _, err := hub.Resume(ctx, st, "prod", "uid-1", "agent-run"); err != nil {
		t.Fatal(err)
	}
	b2 := &Backfill{Cluster: "prod", ClusterUID: "uid-1", Push: push, Resources: b.Resources}
	b2.Read(strings.NewReader(api1))
	if _, err := b2.Run(ctx); err == nil || !strings.Contains(err.Error(), "before the first agent run") {
		t.Errorf("backfill after an agent ran: %v, want refused", err)
	}
}
