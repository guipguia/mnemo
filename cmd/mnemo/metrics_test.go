package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
	"github.com/guipguia/mnemo/internal/webhook"
)

func TestMetrics(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b := hub.Batch{Cluster: `we"ird`, ClusterUID: "u", Items: []hub.Item{{Change: store.Change{
		Ref: store.Ref{Kind: "ConfigMap", Namespace: "n", Name: "x"}, Time: time.Now(), Object: []byte(`{"a":1}`)}}}}
	if err := hub.Ingest(ctx, st, b); err != nil {
		t.Fatal(err)
	}
	hub.Ingest(ctx, st, hub.Batch{}) // rejected: no cluster
	st.SetWebhookCursor(ctx, "slack", 0)
	hooks := &webhook.Config{Webhooks: []webhook.Webhook{{Name: "slack"}, {Name: "new"}}}

	rec := httptest.NewRecorder()
	metrics(st, hooks).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		"mnemo_changes_recorded_total 1\n",
		"mnemo_ingest_failures_total 1\n",
		`mnemo_cluster_last_seen_timestamp_seconds{cluster="we\"ird"} `,
		`mnemo_webhook_pending_changes{webhook="slack"} 1` + "\n",
		`mnemo_webhook_position{webhook="slack"} 0` + "\n",
		"# TYPE mnemo_db_size_bytes gauge\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `webhook="new"`) { // no position yet: not started
		t.Errorf("unstarted webhook reported:\n%s", out)
	}
}
