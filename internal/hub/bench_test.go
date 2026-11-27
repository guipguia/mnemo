package hub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/guipguia/mnemo/internal/store"
)

// BenchmarkIngestHTTP is an agent pushing to the hub end to end: JSON over HTTP with the
// token, batches of 100 modified objects with managedFields, attribution, and storage
// (SQLite). See docs/benchmarks.md.
//
//	go test ./internal/hub -run '^$' -bench IngestHTTP -benchtime 200x
func BenchmarkIngestHTTP(b *testing.B) {
	st, err := store.Open(filepath.Join(b.TempDir(), "mnemo.db"))
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
