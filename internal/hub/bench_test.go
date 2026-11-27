package hub

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/guipguia/mnemo/internal/store"
)

// pgSchema creates a schema in the PostgreSQL at dsn, drops it when b ends, and returns a
// DSN that uses it.
func pgSchema(b *testing.B, dsn string) string {
	b.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		b.Fatal(err)
	}
	name := fmt.Sprintf("bench_%d", time.Now().UnixNano())
	if _, err := db.Exec("CREATE SCHEMA " + name); err != nil {
		db.Close()
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Exec("DROP SCHEMA " + name + " CASCADE"); db.Close() })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + name
}

// BenchmarkIngestHTTP is an agent pushing to the hub end to end: JSON over HTTP with the
// token, batches of 100 modified objects with managedFields, attribution, and storage
// (SQLite, or PostgreSQL with $MNEMO_TEST_POSTGRES, in a schema of its own). See
// docs/benchmarks.md.
//
//	go test ./internal/hub -run '^$' -bench IngestHTTP -benchtime 200x
func BenchmarkIngestHTTP(b *testing.B) {
	dsn := filepath.Join(b.TempDir(), "mnemo.db")
	if pg := os.Getenv("MNEMO_TEST_POSTGRES"); pg != "" {
		dsn = pgSchema(b, pg)
	}
	st, err := store.Open(dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(Handler(st, "t", func(*http.Request) store.Access { return store.Access{} }))
	defer srv.Close()
	c := NewClient(srv.URL, "t", nil)
	managers := deploymentManagers()
	t0 := time.Now()
	batch := 0
	for b.Loop() {
		bt := Batch{Cluster: "prod", ClusterUID: "u"}
		for i := range 100 {
			name := fmt.Sprint("api-", i)
			obj := fmt.Appendf(nil, `{"metadata":{"name":%q,"namespace":"shop","labels":{"app":%q}},
				"spec":{"replicas":%d,"template":{"spec":{"containers":[{"name":"app","image":"nginx:1.%d"}]}}}}`,
				name, name, 1+batch%5, batch)
			bt.Items = append(bt.Items, Item{Change: store.Change{Ref: store.Ref{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: name},
				Time: t0.Add(time.Duration(batch) * time.Second), Object: obj}, Managers: managers})
		}
		if err := c.Push(b.Context(), bt); err != nil {
			b.Fatal(err)
		}
		batch++
	}
	b.ReportMetric(float64(100*b.N)/b.Elapsed().Seconds(), "objects/s")
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	b.ReportMetric(float64(m.HeapInuse)/1e6, "heap-MB")
}
