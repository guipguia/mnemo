package agent

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestClassify(t *testing.T) {
	for manager, want := range map[string]string{
		"":                          "unknown",
		"argocd-controller":         "gitops",
		"kustomize-controller":      "gitops",
		"kubectl-edit":              "manual",
		"kubectl-client-side-apply": "manual",
		"helm":                      "manual",
		"Mozilla":                   "manual",
		"kube-controller-manager":   "controller",
	} {
		if got := classify(manager); got != want {
			t.Errorf("classify(%q) = %q, want %q", manager, got, want)
		}
	}
}

func TestToItem(t *testing.T) {
	t1 := metav1.NewTime(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	t2 := metav1.NewTime(t1.Add(time.Hour))
	t3 := metav1.NewTime(t1.Add(2 * time.Hour))
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "api", "namespace": "shop", "uid": "u1", "resourceVersion": "99", "generation": int64(4),
			"labels":      map[string]any{"argocd.argoproj.io/instance": "shop"},
			"annotations": map[string]any{"kubectl.kubernetes.io/last-applied-configuration": "{}"},
		},
		"spec":   map[string]any{"replicas": int64(3)},
		"status": map[string]any{"readyReplicas": int64(3)},
	}}
	u.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: "argocd-controller", Time: &t1},
		{Manager: "kubectl-edit", Time: &t2},
		{Manager: "kube-controller-manager", Subresource: "status", Time: &t3}, // newer, but status: ignored
	})

	now := time.Now()
	it, err := toItem(u, true, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if it.Author != "kubectl-edit" || it.Source != "manual" || !it.Drift {
		t.Errorf("author=%q source=%q drift=%v, want kubectl-edit/manual/drift", it.Author, it.Source, it.Drift)
	}
	if !it.Time.Equal(t2.Time) {
		t.Errorf("listed time = %v, want last manager time %v", it.Time, t2.Time)
	}
	if it.Kind != "Deployment" || it.Group != "apps" || it.Version != "v1" || it.ResourceVersion != "99" {
		t.Errorf("identity: %+v", it.Change)
	}
	want := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"labels":{"argocd.argoproj.io/instance":"shop"},"name":"api","namespace":"shop","uid":"u1"},"spec":{"replicas":3}}`
	if string(it.Object) != want {
		t.Errorf("object =\n%s\nwant\n%s", it.Object, want)
	}
	if u.Object["status"] == nil {
		t.Error("normalize must not mutate the input")
	}

	if it, _ := toItem(u, false, false, now); !it.Time.Equal(now) {
		t.Errorf("watch event time = %v, want now", it.Time)
	}
	if it, _ := toItem(u, false, true, now); it.Object != nil || it.Source != "unknown" {
		t.Errorf("deleted: object=%s source=%q", it.Object, it.Source)
	}
}
