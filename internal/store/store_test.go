package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ref := Ref{Cluster: "prod", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "api"}
	t0 := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

	record := func(m int, obj string, listed, wantStored bool, wantType Type) *Change {
		t.Helper()
		c := &Change{Ref: ref, Time: at(m), Author: "kubectl-edit", Source: "manual"}
		if obj != "" {
			c.Object = []byte(obj)
		}
		stored, err := s.Record(ctx, c, listed)
		if err != nil {
			t.Fatal(err)
		}
		if stored != wantStored || (stored && c.Type != wantType) {
			t.Fatalf("minute %d: stored=%v type=%q, want stored=%v type=%q", m, stored, c.Type, wantStored, wantType)
		}
		return c
	}

	record(0, `{"spec":{"replicas":1},"metadata":{"annotations":{"app.kubernetes.io/name":"api"}}}`, true, true, Baseline)
	// Same object, different key order: canonicalized, so not a change.
	record(1, `{"metadata":{"annotations":{"app.kubernetes.io/name":"api"}},"spec":{"replicas":1}}`, false, false, "")
	c := record(2, `{"spec":{"replicas":3},"metadata":{"annotations":{"app.kubernetes.io/name":"shop"}}}`, false, true, Modified)
	if want := []string{"/metadata/annotations/app.kubernetes.io~1name", "/spec/replicas"}; !slices.Equal(c.Paths, want) {
		t.Fatalf("paths = %v, want %v", c.Paths, want)
	}
	record(3, "", false, true, Deleted)
	record(4, "", false, false, "")                         // deleting twice is a no-op
	record(5, `{"spec":{"replicas":1}}`, true, true, Added) // re-created while offline: not a baseline

	if _, err := s.At(ctx, ref, at(-1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before first record: err = %v, want ErrNotFound", err)
	}
	if got, err := s.At(ctx, ref, at(2).Add(30*time.Second)); err != nil || string(got.Object) != `{"metadata":{"annotations":{"app.kubernetes.io/name":"shop"}},"spec":{"replicas":3}}` {
		t.Fatalf("at 14:02:30: %s, %v", got.Object, err)
	}
	if _, err := s.At(ctx, ref, at(3)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}

	other := &Change{Ref: Ref{Cluster: "staging", Kind: "ConfigMap", Namespace: "shop", Name: "cfg"}, Time: at(2), Object: []byte(`{}`)}
	if _, err := s.Record(ctx, other, false); err != nil {
		t.Fatal(err)
	}

	feed, err := s.Changes(ctx, Filter{Cluster: "prod", Since: at(2)})
	if err != nil {
		t.Fatal(err)
	}
	var types []Type
	for _, c := range feed {
		if c.Object != nil {
			t.Fatal("feed must not load object bodies")
		}
		types = append(types, c.Type)
	}
	if want := []Type{Added, Deleted, Modified}; !slices.Equal(types, want) {
		t.Fatalf("feed types = %v, want %v", types, want)
	}

	hist, err := s.History(ctx, ref, 0)
	if err != nil || len(hist) != 4 {
		t.Fatalf("history: %d entries, %v; want 4", len(hist), err)
	}
	got, err := s.Get(ctx, hist[0].ID)
	if err != nil || string(got.Object) != `{"spec":{"replicas":1}}` {
		t.Fatalf("get latest: %s, %v", got.Object, err)
	}
}

func TestClustersCheckpointsPrune(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.RegisterCluster(ctx, "prod", "uid-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterCluster(ctx, "prod", "uid-1"); err != nil {
		t.Fatalf("re-register same cluster: %v", err)
	}
	if err := s.RegisterCluster(ctx, "prod", "uid-2"); !errors.Is(err, ErrClusterConflict) {
		t.Fatalf("name reuse: err = %v, want ErrClusterConflict", err)
	}

	if err := s.SetCheckpoint(ctx, "prod", "apps/v1/deployments", "10"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCheckpoint(ctx, "prod", "apps/v1/deployments", "20"); err != nil {
		t.Fatal(err)
	}
	if cp, err := s.Checkpoints(ctx, "prod"); err != nil || cp["apps/v1/deployments"] != "20" {
		t.Fatalf("checkpoints = %v, %v", cp, err)
	}

	now := time.Now()
	for _, name := range []string{"a", "b", "c"} {
		c := &Change{Ref: Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "ns", Name: name}, Time: now, Object: []byte(`{}`), Drift: name == "a"}
		if _, err := s.Record(ctx, c, true); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Prune(ctx, "prod", "", "ConfigMap", map[string]bool{"ns/a": true}, now.Add(time.Minute))
	if err != nil || n != 2 {
		t.Fatalf("prune = %d, %v; want 2", n, err)
	}
	if n, _ := s.Prune(ctx, "prod", "", "ConfigMap", map[string]bool{"ns/a": true}, now.Add(time.Minute)); n != 0 {
		t.Fatalf("second prune = %d, want 0", n)
	}
	drift, err := s.Changes(ctx, Filter{DriftOnly: true})
	if err != nil || len(drift) != 1 || drift[0].Name != "a" {
		t.Fatalf("drift feed = %v, %v", drift, err)
	}
}
