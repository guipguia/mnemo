package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	s, err := openTest(t)
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
		stored, err := s.Record(ctx, c, listed, nil)
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
	if _, err := s.Record(ctx, other, false, nil); err != nil {
		t.Fatal(err)
	}

	feed, err := s.Changes(ctx, Filter{Access: Access{All: true}, Cluster: "prod", Since: at(2)})
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
	s, err := openTest(t)
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
		if _, err := s.Record(ctx, c, true, nil); err != nil {
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
	drift, err := s.Changes(ctx, Filter{Access: Access{All: true}, DriftOnly: true})
	if err != nil || len(drift) != 1 || drift[0].Name != "a" {
		t.Fatalf("drift feed = %v, %v", drift, err)
	}
}

func TestAccess(t *testing.T) {
	ctx := context.Background()
	s, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	objects := []Ref{
		{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "a"},
		{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop-api", Name: "b"},
		{Cluster: "prod", Kind: "ConfigMap", Namespace: "payments", Name: "c"},
		{Cluster: "prod", Kind: "ClusterRole", Name: "d"}, // cluster-scoped
		{Cluster: "prod-eu", Kind: "ConfigMap", Namespace: "shop", Name: "e"},
		{Cluster: "staging", Kind: "ConfigMap", Namespace: "shop", Name: "f"},
		{Cluster: "prod", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "g"},
	}
	for _, r := range objects {
		if _, err := s.Record(ctx, &Change{Ref: r, Time: time.Now(), Object: []byte(`{}`)}, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		a    Access
		want string
	}{
		{"zero value sees nothing", Access{}, ""},
		{"all", Access{All: true}, "abcdefg"},
		{"one cluster, everything in it", Access{Scopes: []Scope{{Clusters: []string{"prod"}}}}, "abcdg"},
		{"cluster prefix", Access{Scopes: []Scope{{Clusters: []string{"prod*"}}}}, "abcdeg"},
		{"namespace prefix", Access{Scopes: []Scope{{Clusters: []string{"prod"}, Namespaces: []string{"shop*"}}}}, "abg"},
		{"namespace * excludes cluster-scoped", Access{Scopes: []Scope{{Clusters: []string{"prod"}, Namespaces: []string{"*"}}}}, "abcg"},
		{"union of scopes", Access{Scopes: []Scope{{Namespaces: []string{"payments"}}, {Clusters: []string{"staging"}}}}, "cf"},
		{"core resource", Access{Scopes: []Scope{{Clusters: []string{"prod"}, Resources: Resources{{"", "configmaps"}}}}}, "abc"},
		{"resource in any group", Access{Scopes: []Scope{{Resources: Resources{{"*", "deployments"}}}}}, "g"},
		{"every resource of a group", Access{Scopes: []Scope{{Resources: Resources{{"apps", "*"}}}}}, "g"},
		{"everything", Access{Scopes: []Scope{{Clusters: []string{"prod"}, Resources: Resources{{"", "configmaps"}, {"*", "*"}}}}}, "abcdg"},
		{"cluster-scoped resource in a namespace", Access{Scopes: []Scope{{Namespaces: []string{"shop"}, Resources: Resources{{"", "clusterroles"}}}}}, ""},
		{"cluster-scoped resource", Access{Scopes: []Scope{{Resources: Resources{{"", "clusterroles"}}}}}, "d"},
		{"subresource", Access{Scopes: []Scope{{Resources: Resources{{"", "configmaps/status"}}}}}, ""},
	} {
		feed, err := s.Changes(ctx, Filter{Access: tc.a})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range feed {
			got = append(got, c.Name)
			if !tc.a.Allows(c.Ref) {
				t.Errorf("%s: SQL returned %s but Allows denies it", tc.name, c.Name)
			}
		}
		slices.Sort(got)
		if strings.Join(got, "") != tc.want {
			t.Errorf("%s: feed = %v, want %q", tc.name, got, tc.want)
		}
		// Allows must agree with SQL for denied objects too.
		for _, r := range objects {
			if tc.a.Allows(r) != strings.Contains(tc.want, r.Name) {
				t.Errorf("%s: Allows(%s/%s) disagrees with SQL", tc.name, r.Cluster, r.Namespace)
			}
		}
	}
}

func TestRecordKeepsOrder(t *testing.T) {
	ctx := context.Background()
	s, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ref := Ref{Cluster: "c", Kind: "ConfigMap", Namespace: "n", Name: "x"}
	t0 := time.Date(2026, 10, 21, 14, 32, 10, 500_000_000, time.UTC)
	if _, err := s.Record(ctx, &Change{Ref: ref, Time: t0, Object: []byte(`{"a":1}`)}, false, nil); err != nil {
		t.Fatal(err)
	}
	// A corrected time truncated to the second would sort before the previous version.
	c := &Change{Ref: ref, Time: t0.Truncate(time.Second), Object: []byte(`{"a":2}`)}
	if _, err := s.Record(ctx, c, false, nil); err != nil {
		t.Fatal(err)
	}
	if !c.Time.Equal(t0) {
		t.Errorf("time = %v, want clamped to previous version's %v", c.Time, t0)
	}
	if got, _ := s.At(ctx, ref, t0); string(got.Object) != `{"a":2}` {
		t.Errorf("state at t0 = %s, want the newer version", got.Object)
	}
}

func TestExpire(t *testing.T) {
	ctx := context.Background()
	s, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return t0.AddDate(0, 0, d) }
	record := func(name string, d int, obj string) {
		t.Helper()
		c := &Change{Ref: Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: name}, Time: day(d)}
		if obj != "" {
			c.Object = []byte(obj)
		}
		if _, err := s.Record(ctx, c, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	record("old", 0, `{"v":1}`) // only version: kept however old
	record("gone", 0, `{"v":1}`)
	record("gone", 1, "") // deleted long ago: dropped entirely
	record("busy", 0, `{"v":1}`)
	record("busy", 1, `{"v":2}`)
	record("busy", 20, `{"v":3}`)
	record("busy", 21, `{"v":4}`)
	record("busy", 22, `{"v":5}`)

	names := func() map[string]int {
		cs, err := s.Changes(ctx, Filter{Access: Access{All: true}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, c := range cs {
			out[c.Name]++
		}
		return out
	}
	if n, err := s.Expire(ctx, day(10), 0); err != nil || n != 4 {
		t.Fatalf("expire by age: deleted %d, %v; want 4", n, err)
	}
	if got := names(); got["old"] != 1 || got["busy"] != 3 || got["gone"] != 0 {
		t.Fatalf("after age expiry: %v", got)
	}
	if n, err := s.Expire(ctx, time.Time{}, 2); err != nil || n != 1 {
		t.Fatalf("expire by count: deleted %d, %v; want 1", n, err)
	}
	if got, err := s.At(ctx, Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "busy"}, day(30)); err != nil || string(got.Object) != `{"v":5}` {
		t.Fatalf("current state after expiry: %s, %v", got.Object, err)
	}
	if got := names(); got["busy"] != 2 {
		t.Fatalf("after count expiry: %v", got)
	}
	if n, _ := s.Expire(ctx, time.Time{}, 0); n != 0 {
		t.Fatalf("no policy deleted %d rows", n)
	}

	// The newest change overall survives even as an old tombstone, so its ID is never reused.
	record("last", 0, `{"v":1}`)
	record("last", 1, "")
	last, _ := s.LastID(ctx)
	if n, err := s.Expire(ctx, day(10), 0); err != nil || n != 1 {
		t.Fatalf("expire with old newest tombstone: deleted %d, %v; want 1", n, err)
	}
	if id, _ := s.LastID(ctx); id != last {
		t.Fatalf("last ID %d, want %d", id, last)
	}
}

func TestPlural(t *testing.T) {
	for kind, want := range map[string]string{
		"ConfigMap": "configmaps", "Ingress": "ingresses", "NetworkPolicy": "networkpolicies", "Gateway": "gateways",
		"Endpoints": "endpoints", "StorageClass": "storageclasses", "Mesh": "meshes", "Box": "boxes",
	} {
		if got := Plural(kind); got != want {
			t.Errorf("Plural(%s) = %s, want %s", kind, got, want)
		}
		if !slices.Contains(kinds(want), strings.ToLower(kind)) {
			t.Errorf("kinds(%s) = %v, want %s in it", want, kinds(want), strings.ToLower(kind))
		}
	}
}

func TestCurrent(t *testing.T) {
	ctx := context.Background()
	s, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec := func(kind, name, obj string) {
		c := &Change{Ref: Ref{Cluster: "prod", Group: "rbac.authorization.k8s.io", Kind: kind, Name: name}, Time: time.Now()}
		if obj != "" {
			c.Object = []byte(obj)
		}
		if _, err := s.Record(ctx, c, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	rec("ClusterRole", "a", `{"v":1}`)
	rec("ClusterRole", "a", `{"v":2}`)
	rec("ClusterRole", "gone", `{"v":1}`)
	rec("ClusterRole", "gone", "")
	rec("ClusterRoleBinding", "a", `{"v":3}`)
	rec("Other", "x", `{"v":4}`)
	got, err := s.Current(ctx, "prod", "rbac.authorization.k8s.io", "ClusterRole", "ClusterRoleBinding")
	if err != nil {
		t.Fatal(err)
	}
	var objs []string
	for _, c := range got {
		objs = append(objs, c.Kind+" "+c.Name+" "+string(c.Object))
	}
	slices.Sort(objs)
	if want := []string{`ClusterRole a {"v":2}`, `ClusterRoleBinding a {"v":3}`}; !slices.Equal(objs, want) {
		t.Errorf("Current = %v, want %v", objs, want)
	}
}

func TestHeldAudits(t *testing.T) {
	ctx := context.Background()
	s, err := openTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Write(ctx, func(w *Writer) error {
		if ok, err := w.HasHeldAudits(ctx, "prod"); err != nil || ok {
			t.Errorf("HasHeldAudits before any = %v, %v", ok, err)
		}
		for _, a := range []HeldAudit{
			{Namespace: "shop", Name: "cfg", ResourceVersion: "20", User: "carol"},
			{Namespace: "shop", Name: "cfg", UID: "u1", User: "dave"},
		} {
			if err := w.HoldAudit(ctx, "prod", a); err != nil {
				return err
			}
		}
		mod := &Change{Ref: Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "cfg"}, ResourceVersion: "20", UID: "u1", Type: Modified}
		got, err := w.TakeHeldAudits(ctx, mod)
		if err != nil || len(got) != 1 || got[0].User != "carol" {
			t.Errorf("modification: %+v, %v; want carol's", got, err)
		}
		if got, _ := w.TakeHeldAudits(ctx, mod); len(got) != 0 {
			t.Errorf("taken twice: %+v", got)
		}
		del := &Change{Ref: mod.Ref, ResourceVersion: "21", UID: "u1", Type: Deleted}
		if got, err := w.TakeHeldAudits(ctx, del); err != nil || len(got) != 1 || got[0].User != "dave" {
			t.Errorf("deletion: %+v, %v; want dave's", got, err)
		}
		// A delete answered with the object: its audit has the resourceVersion, no UID.
		if err := w.HoldAudit(ctx, "prod", HeldAudit{Namespace: "shop", Name: "cfg", ResourceVersion: "31", User: "erin"}); err != nil {
			return err
		}
		del2 := &Change{Ref: mod.Ref, ResourceVersion: "31", UID: "u2", Type: Deleted}
		if got, err := w.TakeHeldAudits(ctx, del2); err != nil || len(got) != 1 || got[0].User != "erin" {
			t.Errorf("deletion by resourceVersion: %+v, %v; want erin's", got, err)
		}
		// Too old: never applied, and purged.
		if _, err := w.tx.ExecContext(ctx, "INSERT INTO held_audits VALUES ('prod', 'shop', 'old', '40', '', 'zed', '', 1)"); err != nil {
			return err
		}
		if got, _ := w.TakeHeldAudits(ctx, &Change{Ref: Ref{Cluster: "prod", Namespace: "shop", Name: "old"}, ResourceVersion: "40"}); len(got) != 0 {
			t.Errorf("stale held audit applied: %+v", got)
		}
		if err := w.PurgeHeldAudits(ctx); err != nil {
			return err
		}
		var n int
		if err := w.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM held_audits").Scan(&n); err != nil || n != 0 {
			t.Errorf("%d held audits left after the purge (%v)", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
