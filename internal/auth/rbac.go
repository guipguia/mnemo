package auth

import (
	"context"
	"encoding/json"
	"log/slog"
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

	store *store.Store
	mu    sync.Mutex
	at    time.Time
	snap  map[string]*clusterRBAC // by cluster
}

// rbacTTL is how long a snapshot of the recorded RBAC is used, so how long a change to it
// (a revoked binding) takes to apply.
const rbacTTL = 30 * time.Second

type clusterRBAC struct {
	rules    map[string][]rbacv1.PolicyRule // "namespace/name" for Roles, "/name" for ClusterRoles
	bindings []binding
}

type binding struct {
	namespace string // "" for a ClusterRoleBinding
	role      rbacv1.RoleRef
	subjects  []rbacv1.Subject
}

// snapshot returns the recorded RBAC of every cluster, at most rbacTTL old. If reloading
// fails it keeps the previous one (none at first: RBAC then grants nothing).
func (r *RBAC) snapshot() map[string]*clusterRBAC {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.at) < rbacTTL {
		return r.snap
	}
	r.at = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snap, err := r.load(ctx)
	if err != nil {
		slog.Warn("loading recorded RBAC failed; using the previous snapshot", "err", err)
		return r.snap
	}
	r.snap = snap
	return snap
}

func (r *RBAC) load(ctx context.Context) (map[string]*clusterRBAC, error) {
	clusters, err := r.store.Clusters(ctx)
	if err != nil {
		return nil, err
	}
	snap := map[string]*clusterRBAC{}
	for _, cl := range clusters {
		objs, err := r.store.Current(ctx, cl.Name, rbacv1.GroupName, "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding")
		if err != nil {
			return nil, err
		}
		c := &clusterRBAC{rules: map[string][]rbacv1.PolicyRule{}}
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
				c.rules[o.Namespace+"/"+o.Name] = v.Rules
			default:
				c.bindings = append(c.bindings, binding{namespace: o.Namespace, role: v.RoleRef, subjects: v.Subjects})
			}
		}
		snap[cl.Name] = c
	}
	return snap, nil
}

// scopes is what u may read according to each cluster's recorded RBAC.
func (r *RBAC) scopes(u User) []store.Scope {
	name, groups := r.UserPrefix+u.Name, r.groups(u.Groups)
	var out []store.Scope
	for cluster, c := range r.snapshot() {
		if strings.Contains(cluster, "*") {
			continue // would be read as a pattern
		}
		for _, b := range c.bindings {
			if !slices.ContainsFunc(b.subjects, func(s rbacv1.Subject) bool {
				return (s.Kind == rbacv1.UserKind && u.Name != "" && s.Name == name) ||
					(s.Kind == rbacv1.GroupKind && slices.Contains(groups, s.Name))
			}) {
				continue
			}
			key := "/" + b.role.Name
			if b.role.Kind == "Role" {
				if b.namespace == "" {
					continue // invalid: a ClusterRoleBinding can't use a Role
				}
				key = b.namespace + "/" + b.role.Name
			}
			res := readable(c.rules[key])
			if len(res) == 0 {
				continue
			}
			s := store.Scope{Clusters: []string{cluster}, Resources: res}
			if b.namespace != "" { // a RoleBinding grants its namespace only, never cluster-scoped objects
				s.Namespaces = []string{b.namespace}
			}
			out = append(out, s)
		}
	}
	return out
}

func (r *RBAC) groups(gs []string) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = r.GroupPrefix + g
	}
	return out
}

// bound keeps the groups (without prefix) that some recorded binding names.
func (r *RBAC) bound(gs []string) []string {
	var out []string
	snap := r.snapshot()
	for _, g := range gs {
		pg := r.GroupPrefix + g
	clusters:
		for _, c := range snap {
			for _, b := range c.bindings {
				if slices.ContainsFunc(b.subjects, func(s rbacv1.Subject) bool { return s.Kind == rbacv1.GroupKind && s.Name == pg }) {
					out = append(out, g)
					break clusters
				}
			}
		}
	}
	return out
}

// readable is the resources that rules let one get, list or watch. Rules limited to
// resourceNames are skipped (they grant less than a kind), and so are subresources.
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
	return out
}
