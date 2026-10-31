package agent

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestToDeploys(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	argo := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "shop", "namespace": "argocd"},
		"status": map[string]any{"history": []any{
			map[string]any{"id": int64(1), "revision": "aaa", "source": map[string]any{"repoURL": "https://github.com/acme/shop"},
				"deployStartedAt": "2026-10-20T14:00:00Z", "deployedAt": "2026-10-20T14:01:00Z"},
			map[string]any{"id": int64(2), "revisions": []any{"bbb", "ccc"},
				"sources":    []any{map[string]any{"repoURL": "https://github.com/acme/a"}, map[string]any{"repoURL": "https://github.com/acme/b"}},
				"deployedAt": "2026-10-20T14:10:00Z"},
			map[string]any{"id": int64(3), "revision": "ddd"}, // no deployedAt: not finished, skipped
		}},
	}}
	ds := toDeploys(argoApps, argo, now)
	if len(ds) != 2 || ds[0].Key != "1" || ds[0].Revision != "aaa" || ds[0].Repo != "https://github.com/acme/shop" ||
		ds[0].Start.IsZero() || ds[0].End.Hour() != 14 || ds[1].Revision != "bbb" || ds[1].Repo != "https://github.com/acme/a" || !ds[1].Start.IsZero() {
		t.Errorf("argo: %+v", ds)
	}

	flux := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "apps", "namespace": "flux-system"},
		"status":   map[string]any{"lastAppliedRevision": "main@sha1:0123"},
	}}
	if ds := toDeploys(fluxKustomizations, flux, now); len(ds) != 1 || ds[0].Revision != "main@sha1:0123" || !ds[0].End.Equal(now) || ds[0].Key != "" {
		t.Errorf("flux: %+v", ds)
	}
	if ds := toDeploys(configmaps, flux, now); ds != nil {
		t.Errorf("other kinds: %+v", ds)
	}
}
