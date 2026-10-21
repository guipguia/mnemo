package agent

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestToItem(t *testing.T) {
	t1 := metav1.NewTime(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	t2 := metav1.NewTime(t1.Add(time.Hour))
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "api", "namespace": "shop", "uid": "u1", "resourceVersion": "99", "generation": int64(4),
			"annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": "{}", "deployment.kubernetes.io/revision": "4"},
		},
		"spec":   map[string]any{"replicas": int64(3)},
		"status": map[string]any{"readyReplicas": int64(3)},
	}}
	u.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "kubectl-edit", Time: &t1},
		{Manager: "kubectl", Subresource: "scale"},                             // no time, as the API server sends it
		{Manager: "kube-controller-manager", Subresource: "status", Time: &t2}, // newer, but status: ignored
	})

	now := time.Now()
	it, err := toItem(u, true, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if !it.Time.Equal(t1.Time) {
		t.Errorf("listed time = %v, want newest non-status manager time %v", it.Time, t1.Time)
	}
	if len(it.Managers) != 3 || it.Kind != "Deployment" || it.Group != "apps" || it.Version != "v1" || it.ResourceVersion != "99" {
		t.Errorf("item: %+v", it)
	}
	want := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"shop","uid":"u1"},"spec":{"replicas":3}}`
	if string(it.Object) != want {
		t.Errorf("object =\n%s\nwant\n%s", it.Object, want)
	}
	if u.Object["status"] == nil {
		t.Error("normalize must not mutate the input")
	}

	if it, _ := toItem(u, false, false, now); !it.Time.Equal(now) {
		t.Errorf("watch event time = %v, want now", it.Time)
	}
	if it, _ := toItem(u, false, true, now); it.Object != nil || it.Managers != nil {
		t.Errorf("deleted: object=%s managers=%v", it.Object, it.Managers)
	}
}
