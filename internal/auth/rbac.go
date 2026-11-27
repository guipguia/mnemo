package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/guipguia/mnemo/internal/store"
)

// RBAC grants each user what the Kubernetes RBAC recorded for each cluster lets them read
// (get, list or watch), kind by kind. The hub has no credentials for the clusters, so it
// evaluates the latest recorded Roles, ClusterRoles and their bindings itself.
//
//	rbac:
//	  userPrefix: "oidc:"    # as the API server's --oidc-username-prefix
//	  groupPrefix: "oidc:"   # as --oidc-groups-prefix
type RBAC struct {
	UserPrefix  string `json:"userPrefix,omitempty"`
	GroupPrefix string `json:"groupPrefix,omitempty"`

	store   *store.Store
	mu      sync.Mutex
	at      time.Time
	loading bool
	snap    []clusterRBAC // sorted by cluster
}

// rbacTTL is how long a snapshot of the recorded RBAC is used, so how long a change to it
// (a revoked binding) takes to apply.
const rbacTTL = 30 * time.Second

type clusterRBAC struct {
	cluster  string
	bindings []binding
}

// binding is a recorded binding with what its role lets one read, worked out at load.
type binding struct {
	namespace string // "" for a ClusterRoleBinding
	subjects  []rbacv1.Subject
	resources store.Resources // sorted
}

// snapshot returns the recorded RBAC of every cluster. Past rbacTTL it starts one reload
// in the background and keeps serving the current snapshot meanwhile.
func (r *RBAC) snapshot() []clusterRBAC {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.at) >= rbacTTL && !r.loading {
		r.loading = true
		go r.reload()
	}
	return r.snap
}

// reload loads the snapshot. If that fails it keeps the previous one (none at first: RBAC
// then grants nothing until a reload succeeds).
func (r *RBAC) reload() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snap, err := r.load(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loading, r.at = false, time.Now()
	if err != nil {
		slog.Warn("loading recorded RBAC failed; using the previous snapshot", "err", err)
		return
	}
	r.snap = snap
}

func (r *RBAC) load(ctx context.Context) ([]clusterRBAC, error) {
	clusters, err := r.store.Clusters(ctx)
	if err != nil {
		return nil, err
	}
	var snap []clusterRBAC
	for _, cl := range clusters {
		if strings.Contains(cl.Name, "*") {
			continue // would be read as a pattern
		}
		objs, err := r.store.Current(ctx, cl.Name, rbacv1.GroupName, "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding")
		if err != nil {
			return nil, err
		}
		roles := map[string]store.Resources{} // "namespace/name" for Roles, "/name" for ClusterRoles
		type raw struct {
			namespace string
			subjects  []rbacv1.Subject
			role      rbacv1.RoleRef
		}
		var bindings []raw
		for _, o := range objs {
			var v struct {
				Rules    []rbacv1.PolicyRule `json:"rules"`
				Subjects []rbacv1.Subject    `json:"subjects"`
				RoleRef  rbacv1.RoleRef      `json:"roleRef"`
			}
			if err := json.Unmarshal(o.Object, &v); err != nil {
				slog.Warn("skipping unreadable RBAC object", "cluster", cl.Name, "kind", o.Kind, "name", o.Name, "err", err)
				continue
			}
			switch o.Kind {
			case "Role", "ClusterRole":
				roles[o.Namespace+"/"+o.Name] = readable(v.Rules)
			default:
				if !strings.Contains(o.Namespace, "*") { // impossible in Kubernetes; would be read as a pattern
					bindings = append(bindings, raw{o.Namespace, v.Subjects, v.RoleRef})
				}
			}
		}
		c := clusterRBAC{cluster: cl.Name}
		for _, b := range bindings {
			key := "/" + b.role.Name
			if b.role.Kind == "Role" {
				if b.namespace == "" {
					continue // invalid: a ClusterRoleBinding can't use a Role
				}
				key = b.namespace + "/" + b.role.Name
			}
			if res := roles[key]; len(res) > 0 {
				c.bindings = append(c.bindings, binding{namespace: b.namespace, subjects: b.subjects, resources: res})
			}
		}
		snap = append(snap, c)
	}
	return snap, nil
}

// scopes is what u may read according to each cluster's recorded RBAC: one scope per set
// of resources and namespaces, however many bindings grant it, so the SQL stays small.
func (r *RBAC) scopes(u User) []store.Scope {
	name, groups := r.name(u.Name), r.groups(u.Groups)
	type grant struct {
		namespaces map[string]bool // nil: the whole cluster
		resources  store.Resources
	}
	type merged struct {
		clusters   []string
		namespaces []string
		resources  store.Resources
	}
	var out []*merged
	byKey := map[string]*merged{}
	for _, c := range r.snapshot() {
		grants := map[string]*grant{} // by resource set
		var order []string
		for _, b := range c.bindings {
			if !slices.ContainsFunc(b.subjects, func(s rbacv1.Subject) bool {
				return (s.Kind == rbacv1.UserKind && name != "" && s.Name == name) ||
					(s.Kind == rbacv1.GroupKind && slices.Contains(groups, s.Name))
			}) {
				continue
			}
			k := resKey(b.resources)
			g := grants[k]
			if g == nil {
				g = &grant{namespaces: map[string]bool{}, resources: b.resources}
				grants[k] = g
				order = append(order, k)
			}
			if b.namespace == "" {
				g.namespaces = nil // a ClusterRoleBinding: every namespace and cluster-scoped objects
			} else if g.namespaces != nil {
				g.namespaces[b.namespace] = true
			}
		}
		for _, k := range order {
			g := grants[k]
			var nss []string
			if g.namespaces != nil {
				nss = slices.Sorted(maps.Keys(g.namespaces))
			}
			// Same resources and namespaces in several clusters: one scope.
			mk := k + "|" + strings.Join(nss, ",") + "|" + boolStr(g.namespaces == nil)
			if m := byKey[mk]; m != nil {
				m.clusters = append(m.clusters, c.cluster)
				continue
			}
			m := &merged{clusters: []string{c.cluster}, namespaces: nss, resources: g.resources}
			byKey[mk] = m
			out = append(out, m)
		}
	}
	scopes := make([]store.Scope, len(out))
	for i, m := range out {
		scopes[i] = store.Scope{Clusters: m.clusters, Namespaces: m.namespaces, Resources: m.resources}
	}
	return scopes
}

func boolStr(b bool) string {
	if b {
		return "all"
	}
	return ""
}

func resKey(rs store.Resources) string {
	var b strings.Builder
	for _, x := range rs {
		b.WriteString(x.Group + "/" + x.Resource + ";")
	}
	return b.String()
}

// name is u's Kubernetes username, or "" if it can't be one. Kubernetes reserves system:
// for its own identities, and no authenticator may assert them: without a prefix, an IdP
// group named system:masters would otherwise be cluster-admin everywhere.
func (r *RBAC) name(n string) string {
	if n == "" || strings.HasPrefix(r.UserPrefix+n, "system:") {
		return ""
	}
	return r.UserPrefix + n
}

// groups is u's Kubernetes groups, without reserved ones (see name).
func (r *RBAC) groups(gs []string) []string {
	var out []string
	for _, g := range gs {
		if pg := r.GroupPrefix + g; !strings.HasPrefix(pg, "system:") {
			out = append(out, pg)
		}
	}
	return out
}

// bound keeps the groups (without prefix) that some recorded binding names.
func (r *RBAC) bound(gs []string) []string {
	var out []string
	snap := r.snapshot()
	for _, g := range gs {
		pg := r.groups([]string{g})
		if len(pg) == 0 {
			continue
		}
		if slices.ContainsFunc(snap, func(c clusterRBAC) bool {
			return slices.ContainsFunc(c.bindings, func(b binding) bool {
				return slices.ContainsFunc(b.subjects, func(s rbacv1.Subject) bool { return s.Kind == rbacv1.GroupKind && s.Name == pg[0] })
			})
		}) {
			out = append(out, g)
		}
	}
	return out
}

// readable is the resources that rules let one get, list or watch, sorted. Rules limited
// to resourceNames are skipped (they grant less than a kind), and so are subresources.
func readable(rules []rbacv1.PolicyRule) store.Resources {
	var out store.Resources
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 || !slices.ContainsFunc(rule.Verbs, func(v string) bool {
			return v == "get" || v == "list" || v == "watch" || v == "*"
		}) {
			continue
		}
		for _, g := range rule.APIGroups {
			for _, res := range rule.Resources {
				if x := (store.Resource{Group: g, Resource: res}); !strings.Contains(res, "/") && !slices.Contains(out, x) {
					out = append(out, x)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b store.Resource) int {
		return cmp.Or(strings.Compare(a.Group, b.Group), strings.Compare(a.Resource, b.Resource))
	})
	return out
}
