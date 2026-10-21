package agent

import (
	"encoding/json"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

// ponytail: hardcoded manager lists; make them configurable when someone runs a tool we misclassify.
var gitopsManagers = map[string]bool{
	"argocd-controller":             true,
	"argocd-application-controller": true,
	"kustomize-controller":          true,
	"helm-controller":               true,
}

// Labels/annotations that mean "this object is owned by a GitOps tool".
var gitopsMarkers = []string{
	"argocd.argoproj.io/instance",
	"argocd.argoproj.io/tracking-id",
	"kustomize.toolkit.fluxcd.io/name",
	"helm.toolkit.fluxcd.io/name",
}

// classify maps a field manager to a change source.
func classify(manager string) string {
	switch {
	case manager == "":
		return "unknown"
	case gitopsManagers[manager]:
		return "gitops"
	// Humans: kubectl and its subcommands, the helm CLI, browser-based dashboards.
	case strings.HasPrefix(manager, "kubectl"), manager == "helm", strings.HasPrefix(manager, "Mozilla"):
		return "manual"
	default:
		return "controller"
	}
}

// toItem turns a live object into what the hub stores. deleted objects carry no body:
// their author is unknown, since a delete doesn't update managedFields.
func toItem(u *unstructured.Unstructured, listed, deleted bool, now time.Time) (hub.Item, error) {
	gvk := u.GroupVersionKind()
	it := hub.Item{Listed: listed, Change: store.Change{
		Ref:     store.Ref{Group: gvk.Group, Kind: gvk.Kind, Namespace: u.GetNamespace(), Name: u.GetName()},
		Version: gvk.Version, UID: string(u.GetUID()), ResourceVersion: u.GetResourceVersion(),
		Time: now, Source: "unknown",
	}}
	if deleted {
		return it, nil
	}

	// The most recent non-status managedFields entry is the best "who" a watch can offer.
	var latest metav1.ManagedFieldsEntry
	gitops := false
	for _, m := range u.GetManagedFields() {
		gitops = gitops || gitopsManagers[m.Manager]
		if m.Subresource == "" && m.Time != nil && (latest.Time == nil || !m.Time.Before(latest.Time)) {
			latest = m
		}
	}
	for _, k := range gitopsMarkers {
		_, l := u.GetLabels()[k]
		_, a := u.GetAnnotations()[k]
		gitops = gitops || l || a
	}
	it.Author = latest.Manager
	it.Source = classify(latest.Manager)
	it.Drift = gitops && it.Source == "manual"
	// Watch events happen now. A listed object may have changed long ago: use when its
	// last writer touched it.
	if listed && latest.Time != nil {
		it.Time = latest.Time.Time
	}

	var err error
	it.Object, err = json.Marshal(normalize(u.Object))
	return it, err
}

// normalize drops fields that change without anyone changing the object's meaning.
func normalize(obj map[string]any) map[string]any {
	obj = runtime.DeepCopyJSON(obj)
	delete(obj, "status")
	unstructured.RemoveNestedField(obj, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(obj, "metadata", "managedFields")
	unstructured.RemoveNestedField(obj, "metadata", "generation")
	unstructured.RemoveNestedField(obj, "metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")
	if a, ok, _ := unstructured.NestedMap(obj, "metadata", "annotations"); ok && len(a) == 0 {
		unstructured.RemoveNestedField(obj, "metadata", "annotations")
	}
	return obj
}
