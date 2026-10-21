package agent

import (
	"encoding/json"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

// toItem turns a live object into what the agent sends. Deleted objects carry no body.
func toItem(u *unstructured.Unstructured, listed, deleted bool, now time.Time) (hub.Item, error) {
	gvk := u.GroupVersionKind()
	it := hub.Item{Listed: listed, Change: store.Change{
		Ref:     store.Ref{Group: gvk.Group, Kind: gvk.Kind, Namespace: u.GetNamespace(), Name: u.GetName()},
		Version: gvk.Version, UID: string(u.GetUID()), ResourceVersion: u.GetResourceVersion(),
		Time: now,
	}}
	if deleted {
		return it, nil
	}
	it.Managers = u.GetManagedFields()
	// Watch events happen now. A listed object may have changed long ago: use when its
	// last writer touched it.
	if listed {
		var latest time.Time
		for _, m := range it.Managers {
			if m.Subresource != "status" && m.Time != nil && m.Time.After(latest) {
				latest = m.Time.Time
			}
		}
		if !latest.IsZero() {
			it.Time = latest
		}
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
	// Bumped by the Deployment controller on every rollout; the template change is the real event.
	unstructured.RemoveNestedField(obj, "metadata", "annotations", "deployment.kubernetes.io/revision")
	if a, ok, _ := unstructured.NestedMap(obj, "metadata", "annotations"); ok && len(a) == 0 {
		unstructured.RemoveNestedField(obj, "metadata", "annotations")
	}
	return obj
}
