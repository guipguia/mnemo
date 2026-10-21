package auth

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/guipguia/mnemo/internal/store"
)

// Policy maps users and IdP groups to what they may see. A user matching several
// rules sees the union; a user matching none sees nothing.
//
//	rules:
//	- groups: [sre]                      # everything, everywhere
//	- groups: [team-shop]
//	  users: [alice@example.com]
//	  clusters: [prod, staging]
//	  namespaces: [shop, "shop-*"]       # namespaced objects only
type Policy struct {
	Rules []Rule `json:"rules"`
}

type Rule struct {
	Users       []string `json:"users,omitempty"`
	Groups      []string `json:"groups,omitempty"`
	store.Scope `json:",inline"`
}

func LoadPolicy(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	// Strict: a typo like "namespace:" must fail loudly, not silently grant a whole cluster.
	if err := yaml.UnmarshalStrict(b, &p); err != nil {
		return nil, fmt.Errorf("policy %s: %w", path, err)
	}
	for i, r := range p.Rules {
		if len(r.Users) == 0 && len(r.Groups) == 0 {
			return nil, fmt.Errorf("policy %s: rule %d has no users or groups", path, i+1)
		}
		for _, pat := range slices.Concat(r.Clusters, r.Namespaces) {
			if pat == "" || strings.Contains(strings.TrimSuffix(pat, "*"), "*") {
				return nil, fmt.Errorf("policy %s: rule %d: pattern %q: want a name, \"*\", or \"prefix*\"", path, i+1, pat)
			}
		}
	}
	return &p, nil
}

func (p *Policy) Access(u User) store.Access {
	var a store.Access
	for _, r := range p.Rules {
		if slices.Contains(r.Users, u.Name) || slices.ContainsFunc(r.Groups, func(g string) bool { return slices.Contains(u.Groups, g) }) {
			a.Scopes = append(a.Scopes, r.Scope)
		}
	}
	return a
}

// relevant keeps only groups the policy mentions. IdPs can send hundreds of groups,
// which would overflow the session cookie and grant nothing anyway.
func (p *Policy) relevant(groups []string) []string {
	var out []string
	for _, g := range groups {
		for _, r := range p.Rules {
			if slices.Contains(r.Groups, g) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}
