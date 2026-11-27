package store

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openTest opens an empty store: SQLite, or with $MNEMO_TEST_POSTGRES (a postgres:// URL) a
// fresh schema in that database. These tests are the conformance suite every backend passes;
// CI runs them against both.
func openTest(t *testing.T) (*Store, error) {
	dsn := os.Getenv("MNEMO_TEST_POSTGRES")
	if dsn == "" {
		return Open(filepath.Join(t.TempDir(), "mnemo.db"))
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	name := "t_" + strings.ToLower(rand.Text())
	if _, err := admin.Exec("CREATE SCHEMA " + name); err != nil {
		admin.Close()
		return nil, err
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + name + " CASCADE")
		admin.Close()
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return Open(fmt.Sprintf("%s%ssearch_path=%s", dsn, sep, name))
}

// The rest of the API, which other packages otherwise test only on SQLite.
func TestCursorsClustersReattribute(t *testing.T) {
	ctx := t.Context()
	s, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterCluster(ctx, "prod", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterCluster(ctx, "prod", "u2"); err != ErrClusterConflict {
		t.Errorf("conflict: %v", err)
	}
	if cs, err := s.Clusters(ctx); err != nil || len(cs) != 1 || cs[0].UID != "u1" || cs[0].LastSeen.IsZero() {
		t.Errorf("clusters: %+v, %v", cs, err)
	}

	now := time.Now()
	for i, obj := range []string{`{"a":1}`, `{"a":2}`, ""} {
		c := &Change{Ref: Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "n", Name: "x"}, UID: "uid-x",
			ResourceVersion: fmt.Sprint(10 + i), Time: now.Add(time.Duration(i) * time.Second), Author: "kubectl-edit", Source: "manual"}
		if obj != "" {
			c.Object = []byte(obj)
		}
		if _, err := s.Record(ctx, c, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.LastID(ctx)
	if err != nil || last == 0 {
		t.Fatalf("last id %d, %v", last, err)
	}
	if cs, err := s.After(ctx, 0, last, 10); err != nil || len(cs) != 3 || cs[2].Type != Deleted {
		t.Errorf("after: %+v, %v", cs, err)
	}

	// By resourceVersion for writes, by UID for the delete.
	set := func(c *Change) { c.Author, c.Source, c.Drift = "alice", "manual", true }
	for _, tc := range []struct{ rv, uid string }{{"11", ""}, {"", "uid-x"}, {"", ""}} {
		n, err := s.Reattribute(ctx, "prod", "n", "x", tc.rv, tc.uid, set)
		if want := map[bool]int{true: 1, false: 0}[tc.rv != "" || tc.uid != ""]; err != nil || n != want {
			t.Errorf("reattribute %+v: %d, %v; want %d", tc, n, err, want)
		}
	}
	cs, _ := s.After(ctx, 0, last, 10)
	if cs[0].Author != "kubectl-edit" || cs[1].Author != "alice" || !cs[1].Drift || cs[2].Author != "alice" {
		t.Errorf("authors: %+v", cs)
	}

	if _, ok, err := s.WebhookCursor(ctx, "slack"); ok || err != nil {
		t.Errorf("unknown cursor: %v, %v", ok, err)
	}
	for _, id := range []int64{2, 3} { // set, then update
		if err := s.SetWebhookCursor(ctx, "slack", id); err != nil {
			t.Fatal(err)
		}
	}
	if id, ok, err := s.WebhookCursor(ctx, "slack"); !ok || err != nil || id != 3 {
		t.Errorf("cursor: %d, %v, %v", id, ok, err)
	}
}

func TestEventWithNUL(t *testing.T) {
	s, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	e := Event{UID: "e", Type: "Warning", Reason: "Unhealthy",
		Message: "probe said \x00 nothing", Count: 1, FirstTime: now, LastTime: now}
	if err := s.RecordEvents(t.Context(), "prod", []Event{e}); err != nil {
		t.Fatal(err) // PostgreSQL rejects NUL in text: one bad event must not stall the batch
	}
}
