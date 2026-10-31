package agent

import (
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guipguia/mnemo/internal/store"
)

// GitOps apps are recorded like any object, and also report their deploys so the hub can
// link the changes they make to a Git revision. Only when the user watches them.
var (
	argoApps           = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	fluxKustomizations = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
)

// toDeploys reads an app's deploys from its status: Argo CD keeps its last syncs in
// status.history; Flux only reports the revision it last applied, observed now.
func toDeploys(gvr schema.GroupVersionResource, u *unstructured.Unstructured, now time.Time) []store.Deploy {
	str := func(obj map[string]any, f ...string) string {
		s, _, _ := unstructured.NestedString(obj, f...)
		return s
	}
	tm := func(obj map[string]any, f string) time.Time { t, _ := time.Parse(time.RFC3339, str(obj, f)); return t }
	d := store.Deploy{Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
	switch gvr {
	case argoApps:
		hist, _, _ := unstructured.NestedSlice(u.Object, "status", "history")
		var out []store.Deploy
		for _, h := range hist {
			h, ok := h.(map[string]any)
			if !ok {
				continue
			}
			d := d
			id, _, _ := unstructured.NestedInt64(h, "id")
			d.Key = strconv.FormatInt(id, 10)
			d.Revision, d.Repo = str(h, "revision"), str(h, "source", "repoURL")
			// ponytail: multi-source apps link to their first source only.
			if revs, _, _ := unstructured.NestedStringSlice(h, "revisions"); d.Revision == "" && len(revs) > 0 {
				d.Revision = revs[0]
			}
			if srcs, _, _ := unstructured.NestedSlice(h, "sources"); d.Repo == "" && len(srcs) > 0 {
				if src, ok := srcs[0].(map[string]any); ok {
					d.Repo = str(src, "repoURL")
				}
			}
			d.Start, d.End = tm(h, "deployStartedAt"), tm(h, "deployedAt")
			if d.Revision != "" && !d.End.IsZero() {
				out = append(out, d)
			}
		}
		return out
	case fluxKustomizations:
		// ponytail: no repo URL; it lives on the GitRepository, which the agent doesn't read.
		if d.Revision = str(u.Object, "status", "lastAppliedRevision"); d.Revision != "" {
			d.End = now
			return []store.Deploy{d}
		}
	}
	return nil
}
