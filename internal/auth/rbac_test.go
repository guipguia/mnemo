package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guipguia/mnemo/internal/store"
)

func TestRBAC(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RegisterCluster(ctx, "prod", "uid-1"); err != nil {
		t.Fatal(err)
	}
	record := func(kind, ns, name, obj string) {
		t.Helper()
		c := &store.Change{Ref: store.Ref{Cluster: "prod", Group: "rbac.authorization.k8s.io", Kind: kind, Namespace: ns, Name: name},
			Time: time.Now(), Object: []byte(obj)}
		if obj == "" {
			c.Object = nil
		}
		if _, err := st.Record(ctx, c, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	record("ClusterRole", "", "view", `{"rules":[
		{"apiGroups":[""],"resources":["configmaps","pods","pods/log"],"verbs":["get","list","watch"]},
		{"apiGroups":["apps"],"resources":["deployments"],"verbs":["list"]},
		{"apiGroups":[""],"resources":["secrets"],"verbs":["create"]}]}`)
	record("ClusterRole", "", "one-cm", `{"rules":[{"apiGroups":[""],"resources":["configmaps"],"resourceNames":["a"],"verbs":["get"]}]}`)
	record("ClusterRole", "", "nodes", `{"rules":[{"apiGroups":[""],"resources":["nodes"],"verbs":["*"]}]}`)
	record("Role", "shop", "apps", `{"rules":[{"apiGroups":["*"],"resources":["*"],"verbs":["get"]}]}`)
	record("RoleBinding", "shop", "team-view", `{"roleRef":{"kind":"ClusterRole","name":"view"},"subjects":[{"kind":"Group","name":"oidc:team-shop"}]}`)
	record("RoleBinding", "shop", "alice-apps", `{"roleRef":{"kind":"Role","name":"apps"},"subjects":[{"kind":"User","name":"oidc:alice"}]}`)
	record("RoleBinding", "shop", "eve", `{"roleRef":{"kind":"ClusterRole","name":"one-cm"},"subjects":[{"kind":"User","name":"oidc:eve"}]}`)
	record("ClusterRoleBinding", "", "ops-nodes", `{"roleRef":{"kind":"ClusterRole","name":"nodes"},"subjects":[{"kind":"Group","name":"oidc:ops"}]}`)
	record("ClusterRoleBinding", "", "gone", `{"roleRef":{"kind":"ClusterRole","name":"view"},"subjects":[{"kind":"User","name":"oidc:mallory"}]}`)
	record("ClusterRoleBinding", "", "gone", "") // deleted: grants nothing
	record("ClusterRoleBinding", "", "bad", `{"roleRef":{"kind":"Role","name":"apps"},"subjects":[{"kind":"User","name":"oidc:alice"}]}`)

	record("ClusterRoleBinding", "", "cluster-admin", `{"roleRef":{"kind":"ClusterRole","name":"nodes"},"subjects":[{"kind":"Group","name":"system:masters"}]}`)
	for i := range 300 { // many bindings of one role: merged into one scope, or the SQL overflows
		record("RoleBinding", fmt.Sprint("ns-", i), "team-view", `{"roleRef":{"kind":"ClusterRole","name":"view"},"subjects":[{"kind":"Group","name":"oidc:team-shop"},{"kind":"User","name":"oidc:bob"}]}`)
	}

	p := writePolicy(t, "rbac:\n  userPrefix: 'oidc:'\n  groupPrefix: 'oidc:'\n")
	p.RBAC.store = st
	p.RBAC.reload()
	cm := func(ns string) store.Ref {
		return store.Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: ns, Name: "a"}
	}
	deploy := store.Ref{Cluster: "prod", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "api"}
	node := store.Ref{Cluster: "prod", Kind: "Node", Name: "n1"}
	team := User{"bob", []string{"team-shop"}}
	for _, tc := range []struct {
		name string
		u    User
		r    store.Ref
		want bool
	}{
		{"ClusterRole bound in a namespace", team, cm("shop"), true},
		{"only that namespace", team, cm("payments"), false},
		{"list is enough", team, deploy, true},
		{"not a resource the role names", team, store.Ref{Cluster: "prod", Group: "apps", Kind: "StatefulSet", Namespace: "shop", Name: "db"}, false},
		{"other cluster", team, store.Ref{Cluster: "staging", Kind: "ConfigMap", Namespace: "shop", Name: "a"}, false},
		{"Role with wildcards", User{"alice", nil}, deploy, true},
		{"Role is namespaced", User{"alice", nil}, node, false},
		{"resourceNames rules are skipped", User{"eve", nil}, cm("shop"), false},
		{"ClusterRoleBinding reaches cluster-scoped objects", User{"carol", []string{"ops"}}, node, true},
		{"deleted binding", User{"mallory", nil}, cm("shop"), false},
		{"prefix is required", User{"oidc:alice", nil}, deploy, false},
		{"unbound user", User{"zed", []string{"everyone"}}, cm("shop"), false},
		{"many bindings", team, cm("ns-299"), true},
	} {
		if got := p.Access(tc.u).Allows(tc.r); got != tc.want {
			t.Errorf("%s: %v reading %v = %v, want %v", tc.name, tc.u, tc.r, got, tc.want)
		}
	}

	// The feed agrees.
	if _, err := st.Record(ctx, &store.Change{Ref: cm("shop"), Time: time.Now(), Object: []byte(`{}`)}, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Record(ctx, &store.Change{Ref: cm("payments"), Time: time.Now(), Object: []byte(`{}`)}, false, nil); err != nil {
		t.Fatal(err)
	}
	feed, err := st.Changes(ctx, store.Filter{Access: p.Access(team), Kind: "ConfigMap"})
	if err != nil || len(feed) != 1 || feed[0].Namespace != "shop" {
		t.Errorf("feed for %v = %v, %v; want the shop ConfigMap only", team, feed, err)
	}

	if n := len(p.Access(team).Scopes); n != 1 {
		t.Errorf("%v has %d scopes, want 1 (the bindings merged)", team, n)
	}

	// Kubernetes reserves system: names; without a prefix, an IdP must not be able to claim them.
	bare := writePolicy(t, "rbac: {}\n")
	bare.RBAC.store = st
	bare.RBAC.reload()
	for _, u := range []User{{"x", []string{"system:masters"}}, {"system:admin", nil}} {
		if a := bare.Access(u); len(a.Scopes) > 0 {
			t.Errorf("%v gets %v, want nothing", u, a.Scopes)
		}
	}
	if got := bare.relevant([]string{"system:masters"}); len(got) > 0 {
		t.Errorf("relevant kept %v", got)
	}

	if got := p.relevant([]string{"team-shop", "ops", "unrelated"}); len(got) != 2 {
		t.Errorf("relevant groups = %v, want team-shop and ops", got)
	}
}

func TestPolicyRefusesResources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.yaml")
	os.WriteFile(path, []byte("rules:\n- groups: [x]\n  resources: [{group: '', resource: configmaps}]\n"), 0o600)
	if _, err := LoadPolicy(path); err == nil {
		t.Error("policy with resources loaded, want error")
	}
}
