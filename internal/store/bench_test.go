package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

// Benchmarks behind docs/benchmarks.md. They run on SQLite, or on PostgreSQL with
// $MNEMO_TEST_POSTGRES; $MNEMO_BENCH_N sets how many versions the query benchmarks load.
//
//	go test ./internal/store -run '^$' -bench . -benchtime 2000x

// object is a Deployment-sized object (~2.5 KB normalized) whose i-th version differs from
// the previous in one field, as a typical edit does.
func object(name string, i int) []byte {
	env := ""
	for e := range 12 {
		env += fmt.Sprintf(`{"name":"SETTING_%d","value":"value-%d"},`, e, e)
	}
	return fmt.Appendf(nil, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":%q,"namespace":"shop",
		"labels":{"app":%q,"team":"payments","tier":"backend","app.kubernetes.io/managed-by":"argocd"},
		"annotations":{"deployment.kubernetes.io/revision":"%d","argocd.argoproj.io/tracking-id":"shop:apps/Deployment:shop/%s"}},
		"spec":{"replicas":3,"selector":{"matchLabels":{"app":%q}},"strategy":{"type":"RollingUpdate"},
		"template":{"metadata":{"labels":{"app":%q,"team":"payments"}},"spec":{"serviceAccountName":%q,
		"containers":[{"name":"app","image":"registry.example.com/shop/%s:1.%d.0","ports":[{"containerPort":8080}],
		"env":[%s{"name":"TIMEOUT","value":"%ds"}],
		"resources":{"requests":{"cpu":"250m","memory":"256Mi"},"limits":{"memory":"512Mi"}},
		"readinessProbe":{"httpGet":{"path":"/healthz","port":8080},"periodSeconds":10}}]}}}}`,
		name, name, i, name, name, name, name, name, i, env, 30+i%5)
}

func benchStore(b *testing.B) *Store {
	b.Helper()
	dsn := os.Getenv("MNEMO_TEST_POSTGRES")
	if dsn == "" {
		dsn = b.TempDir() + "/mnemo.db"
	}
	s, err := Open(dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	if s.db.pg {
		// A clean slate in the shared database: benchmarks don't use per-test schemas.
		for _, tbl := range []string{"changes", "clusters", "events", "deploys", "webhook_cursors", "checkpoints"} {
			if _, err := s.db.ExecContext(context.Background(), "TRUNCATE "+tbl); err != nil {
				b.Fatal(err)
			}
		}
	}
	return s
}

// BenchmarkRecord is ingest: one modified version of one of 1000 objects per op, with the
// diff against the previous version, in its own transaction, as the hub stores them.
func BenchmarkRecord(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	t0 := time.Now()
	i := 0
	for b.Loop() {
		name := fmt.Sprint("api-", i%1000)
		c := &Change{Ref: Ref{Cluster: "prod", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: name},
			Time: t0.Add(time.Duration(i) * time.Millisecond), Author: "argocd-controller", Source: "gitops", Object: object(name, i/1000)}
		if _, err := s.Record(ctx, c, false, nil); err != nil {
			b.Fatal(err)
		}
		i++
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "versions/s")
}

// loaded is a store holding n versions: 10 clusters × 20 namespaces × 50 objects, the rest
// as further versions of the same objects, spread over the last 30 days.
func loaded(b *testing.B) (*Store, int) {
	n := 100_000
	if v, err := strconv.Atoi(os.Getenv("MNEMO_BENCH_N")); err == nil {
		n = v
	}
	s := benchStore(b)
	ctx := context.Background()
	start := time.Now().Add(-30 * 24 * time.Hour)
	step := 30 * 24 * time.Hour / time.Duration(n)
	for i := range n {
		obj := i % 10_000
		r := Ref{Cluster: fmt.Sprint("cluster-", obj%10), Group: "apps", Kind: "Deployment",
			Namespace: fmt.Sprint("ns-", obj/10%20), Name: fmt.Sprint("app-", obj/200)}
		source := "gitops"
		if i%20 == 0 {
			source = "manual"
		}
		c := &Change{Ref: r, Time: start.Add(time.Duration(i) * step), Author: "someone", Source: source, Drift: source == "manual",
			Object: object(r.Name, i/10_000)}
		if _, err := s.Record(ctx, c, false, nil); err != nil {
			b.Fatal(err)
		}
	}
	return s, n
}

// BenchmarkQueries loads MNEMO_BENCH_N versions once, then times each read the UI and API make.
func BenchmarkQueries(b *testing.B) {
	s, n := loaded(b)
	ctx := context.Background()
	size, _ := s.Size(ctx)
	b.Logf("%d versions: database %.1f MB, %.0f bytes per version", n, float64(size)/1e6, float64(size)/float64(n))
	all := Access{All: true}
	team := Access{Scopes: []Scope{{Clusters: []string{"cluster-1", "cluster-2"}, Namespaces: []string{"ns-1*"}}}}
	obj := Ref{Cluster: "cluster-3", Group: "apps", Kind: "Deployment", Namespace: "ns-7", Name: "app-12"}
	for _, q := range []struct {
		name string
		f    func() error
	}{
		{"feed/latest-100", func() error { _, err := s.Changes(ctx, Filter{Access: all}); return err }},
		{"feed/cluster-last-hour", func() error {
			_, err := s.Changes(ctx, Filter{Access: all, Cluster: "cluster-4", Since: time.Now().Add(-time.Hour)})
			return err
		}},
		{"feed/drift-only", func() error { _, err := s.Changes(ctx, Filter{Access: all, DriftOnly: true}); return err }},
		{"feed/team-policy", func() error { _, err := s.Changes(ctx, Filter{Access: team}); return err }},
		{"object/history", func() error { _, err := s.History(ctx, obj, 100); return err }},
		{"object/at-time", func() error { _, err := s.At(ctx, obj, time.Now().Add(-15*24*time.Hour)); return err }},
	} {
		b.Run(q.name, func(b *testing.B) {
			for b.Loop() {
				if err := q.f(); err != nil && err != ErrNotFound {
					b.Fatal(err)
				}
			}
		})
	}
}
