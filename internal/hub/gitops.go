package hub

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/guipguia/mnemo/internal/store"
)

// Commit is the Git revision a GitOps change came from, and a link to it when the repo is known.
type Commit struct {
	store.Deploy
	URL string `json:"url,omitempty"`
}

// commitFor finds the deploy that made a GitOps change, or returns nil.
func commitFor(ctx context.Context, s *store.Store, c store.Change) *Commit {
	if c.Source != "gitops" || c.Object == nil {
		return nil
	}
	kind, ns, name := app(c)
	if name == "" {
		return nil
	}
	d, err := s.DeployAt(ctx, c.Cluster, kind, ns, name, c.Time)
	if err != nil {
		return nil // no deploys recorded: apps not watched, or not yet synced
	}
	return &Commit{Deploy: d, URL: commitURL(d.Repo, d.Revision)}
}

// app names the GitOps app that owns c's object, from the labels and annotations each tool
// sets. The namespace is empty when the tool doesn't record it.
func app(c store.Change) (kind, namespace, name string) {
	meta := metadata(c.Object)
	if n := meta.Labels["kustomize.toolkit.fluxcd.io/name"]; n != "" {
		return "Kustomization", meta.Labels["kustomize.toolkit.fluxcd.io/namespace"], n
	}
	// Argo CD tracking: "<app>:<group>/<kind>:<namespace>/<name>" annotation (the default since
	// 3.0), or the app's name in a label. Apps outside Argo CD's namespace are "<namespace>_<app>".
	a, _, _ := strings.Cut(meta.Annotations["argocd.argoproj.io/tracking-id"], ":")
	if a == "" {
		a = meta.Labels["argocd.argoproj.io/instance"]
	}
	if a == "" && strings.HasPrefix(c.Author, "argocd") {
		a = meta.Labels["app.kubernetes.io/instance"] // the legacy default label, also set by Helm
	}
	if a == "" {
		return "", "", ""
	}
	if ns, n, ok := strings.Cut(a, "_"); ok {
		return "Application", ns, n
	}
	return "Application", "", a
}

var sha = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// commitURL links to revision in repo on a Git web host, or returns "" if revision isn't a
// commit (a Helm chart version) or repo isn't a URL we can turn into a web page.
func commitURL(repo, revision string) string {
	if !sha.MatchString(revision) || repo == "" {
		return ""
	}
	// git@host:org/repo.git -> ssh://git@host/org/repo.git
	if !strings.Contains(repo, "://") {
		if at, path, ok := strings.Cut(repo, ":"); ok {
			repo = "ssh://" + at + "/" + path
		}
	}
	u, err := url.Parse(repo)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh") {
		return ""
	}
	path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	commit := "/commit/"
	if u.Hostname() == "bitbucket.org" {
		commit = "/commits/"
	}
	return "https://" + u.Hostname() + path + commit + revision
}
