package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDeploys(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	at := func(m float64) time.Time { return t0.Add(time.Duration(m * float64(time.Minute))) }

	if err := s.RecordDeploys(ctx, "prod", []Deploy{
		// Argo CD: history entries with start and end; resending one updates it.
		{Kind: "Application", Namespace: "argocd", Name: "shop", Key: "1", Revision: "aaa", Start: at(0), End: at(1)},
		{Kind: "Application", Namespace: "argocd", Name: "shop", Key: "2", Revision: "bbb", Start: at(10), End: at(11)},
		{Kind: "Application", Namespace: "argocd", Name: "shop", Key: "2", Revision: "bbb", Repo: "https://git.example.com/shop", Start: at(10), End: at(11)},
		// Flux: the agent sends the applied revision on every status update; only changes count.
		{Kind: "Kustomization", Namespace: "flux-system", Name: "apps", Revision: "main@sha1:111", End: at(5)},
		{Kind: "Kustomization", Namespace: "flux-system", Name: "apps", Revision: "main@sha1:111", End: at(6)},
		{Kind: "Kustomization", Namespace: "flux-system", Name: "apps", Revision: "main@sha1:222", End: at(20)},
		{Kind: "Kustomization", Namespace: "flux-system", Name: "apps", Revision: "main@sha1:111", End: at(30)}, // revert
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		kind, ns, name string
		m              float64
		want           string
	}{
		{"Application", "argocd", "shop", 0.5, "aaa"},                  // during sync 1
		{"Application", "", "shop", 10.5, "bbb"},                       // during sync 2, any namespace
		{"Application", "argocd", "shop", 5, "aaa"},                    // between syncs: drift reverted to aaa
		{"Application", "argocd", "shop", 12, "bbb"},                   // after sync 2
		{"Kustomization", "flux-system", "apps", 4.9, "main@sha1:111"}, // applied just before status
		{"Kustomization", "flux-system", "apps", 19.9, "main@sha1:222"},
		{"Kustomization", "flux-system", "apps", 25, "main@sha1:222"},   // drift revert
		{"Kustomization", "flux-system", "apps", 29.9, "main@sha1:111"}, // the revert to 111
	} {
		d, err := s.DeployAt(ctx, "prod", tc.kind, tc.ns, tc.name, at(tc.m))
		if err != nil || d.Revision != tc.want {
			t.Errorf("%s %s at %v: %q, %v; want %q", tc.kind, tc.name, tc.m, d.Revision, err, tc.want)
		}
	}
	if d, _ := s.DeployAt(ctx, "prod", "Application", "argocd", "shop", at(10.5)); d.Repo != "https://git.example.com/shop" || !d.Start.Equal(at(10)) {
		t.Errorf("resent deploy not updated: %+v", d)
	}
	if _, err := s.DeployAt(ctx, "prod", "Application", "argocd", "shop", at(-20)); !errors.Is(err, ErrNotFound) {
		t.Errorf("before any deploy: %v, want ErrNotFound", err)
	}
}
