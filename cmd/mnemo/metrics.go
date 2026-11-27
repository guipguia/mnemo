package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
	"github.com/guipguia/mnemo/internal/webhook"
)

// metrics serves Prometheus text format, read from the database at scrape time so the
// numbers survive restarts and need no client library. See docs/metrics.md.
// ponytail: hand-written exposition, counters and gauges only; switch to client_golang if we
// need histograms.
func metrics(st *store.Store, hooks *webhook.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		clusters, err := st.Clusters(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		last, err := st.LastID(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		size, err := st.Size(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		var b strings.Builder
		metric := func(name, typ, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ) }

		metric("mnemo_build_info", "gauge", "Always 1; the version label is the running mnemo.")
		fmt.Fprintf(&b, "mnemo_build_info{version=%s} 1\n", quote(version))
		metric("mnemo_changes_recorded_total", "counter", "Changes recorded since the database was created (the last change ID).")
		fmt.Fprintf(&b, "mnemo_changes_recorded_total %d\n", last)
		metric("mnemo_ingest_failures_total", "counter", "Agent batches the hub rejected or failed to store since it started.")
		fmt.Fprintf(&b, "mnemo_ingest_failures_total %d\n", hub.IngestFailures.Load())
		metric("mnemo_db_size_bytes", "gauge", "SQLite database size, excluding the WAL.")
		fmt.Fprintf(&b, "mnemo_db_size_bytes %d\n", size)
		metric("mnemo_cluster_last_seen_timestamp_seconds", "gauge", "When each cluster's agent last pushed or resumed.")
		for _, c := range clusters {
			fmt.Fprintf(&b, "mnemo_cluster_last_seen_timestamp_seconds{cluster=%s} %d\n", quote(c.Name), c.LastSeen.Unix())
		}
		if hooks != nil && len(hooks.Webhooks) > 0 {
			metric("mnemo_webhook_pending_changes", "gauge", "Changes recorded after the webhook's position, matching or not; growth means delivery is stuck.")
			for _, h := range hooks.Webhooks {
				cursor, ok, err := st.WebhookCursor(ctx, h.Name)
				if err != nil {
					http.Error(w, err.Error(), http.StatusServiceUnavailable)
					return
				}
				if ok {
					fmt.Fprintf(&b, "mnemo_webhook_pending_changes{webhook=%s} %d\n", quote(h.Name), last-cursor)
				}
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write([]byte(b.String()))
	})
}

// quote is a label value: strconv.Quote escapes \, " and newlines as the format requires.
func quote(s string) string { return strconv.Quote(s) }
