package hub

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/guipguia/mnemo/internal/store"
)

func TestApp(t *testing.T) {
	for _, tc := range []struct{ author, meta, kind, ns, name string }{
		{"kustomize-controller", `{"labels":{"kustomize.toolkit.fluxcd.io/name":"apps","kustomize.toolkit.fluxcd.io/namespace":"flux-system"}}`, "Kustomization", "flux-system", "apps"},
		{"argocd-controller", `{"annotations":{"argocd.argoproj.io/tracking-id":"shop:apps/Deployment:shop/web"}}`, "Application", "", "shop"},
		{"argocd-controller", `{"annotations":{"argocd.argoproj.io/tracking-id":"team-a_shop:apps/Deployment:shop/web"}}`, "Application", "team-a", "shop"},
		{"argocd-controller", `{"labels":{"argocd.argoproj.io/instance":"shop"}}`, "Application", "", "shop"},
		{"argocd-application-controller", `{"labels":{"app.kubernetes.io/instance":"shop"}}`, "Application", "", "shop"},
		{"helm-controller", `{"labels":{"app.kubernetes.io/instance":"shop"}}`, "", "", ""}, // set by Helm too
	} {
		c := store.Change{Author: tc.author, Object: json.RawMessage(`{"metadata":` + tc.meta + `}`)}
		if kind, ns, name := app(c); kind != tc.kind || ns != tc.ns || name != tc.name {
			t.Errorf("%s %s: %s %s/%s, want %s %s/%s", tc.author, tc.meta, kind, ns, name, tc.kind, tc.ns, tc.name)
		}
	}
}

func TestCommitURL(t *testing.T) {
	const rev = "0123456789abcdef0123456789abcdef01234567"
	for repo, want := range map[string]string{
		"https://github.com/acme/shop.git":    "https://github.com/acme/shop/commit/" + rev,
		"https://gitlab.com/acme/infra/shop/": "https://gitlab.com/acme/infra/shop/commit/" + rev,
		"git@github.com:acme/shop.git":        "https://github.com/acme/shop/commit/" + rev,
		"ssh://git@git.example.com:2222/shop": "https://git.example.com/shop/commit/" + rev,
		"https://bitbucket.org/acme/shop.git": "https://bitbucket.org/acme/shop/commits/" + rev,
		"oci://ghcr.io/acme/charts":           "",
		"":                                    "",
	} {
		if got := commitURL(repo, rev); got != want {
			t.Errorf("%q: %q, want %q", repo, got, want)
		}
	}
	if got := commitURL("https://github.com/acme/shop", "1.2.3"); got != "" {
		t.Errorf("chart version linked: %q", got)
	}
}

func TestCommitFor(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	const rev = "0123456789abcdef0123456789abcdef01234567"
	if err := Ingest(ctx, s, Batch{Cluster: "prod", ClusterUID: "uid", Items: []Item{{
		Change: store.Change{
			Ref:  store.Ref{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web"},
			Time: t0.Add(30 * time.Second), Object: json.RawMessage(`{"metadata":{"labels":{"argocd.argoproj.io/instance":"shop"}}}`),
		},
		Managers: []metav1.ManagedFieldsEntry{{Manager: "argocd-controller"}},
	}}, Deploys: []store.Deploy{{Kind: "Application", Namespace: "argocd", Name: "shop", Key: "1",
		Revision: rev, Repo: "https://github.com/acme/shop.git", Start: t0, End: t0.Add(time.Minute)}},
	}); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Changes(ctx, store.Filter{Access: store.Access{All: true}})
	c, _ := s.Get(ctx, cs[0].ID)
	if got := commitFor(ctx, s, c); got == nil || got.Revision != rev || got.URL != "https://github.com/acme/shop/commit/"+rev {
		t.Fatalf("commit = %+v", got)
	}
	c.Source = "manual"
	if got := commitFor(ctx, s, c); got != nil {
		t.Fatalf("manual change got a commit: %+v", got)
	}
}
