package hub

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/guipguia/mnemo/internal/store"
)

func TestClassify(t *testing.T) {
	for manager, want := range map[string]string{
		"":                          "unknown",
		"argocd-controller":         "gitops",
		"kustomize-controller":      "gitops",
		"kubectl":                   "manual",
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

// Managers as a real cluster reports them after create, scale, and set image.
func deploymentManagers() []metav1.ManagedFieldsEntry {
	t1 := metav1.NewTime(time.Date(2026, 10, 21, 18, 15, 13, 0, time.UTC))
	t2 := metav1.NewTime(t1.Add(2 * time.Second))
	t3 := metav1.NewTime(t1.Add(4 * time.Second))
	f := func(s string) *metav1.FieldsV1 { return &metav1.FieldsV1{Raw: []byte(s)} }
	return []metav1.ManagedFieldsEntry{
		{Manager: "kubectl", Operation: "Update", Subresource: "scale", FieldsV1: f(`{"f:spec":{"f:replicas":{}}}`)},
		{Manager: "kubectl-create", Operation: "Update", Time: &t1, FieldsV1: f(`{"f:metadata":{"f:labels":{".":{},"f:app":{}}},"f:spec":{"f:selector":{},"f:template":{"f:metadata":{"f:labels":{".":{},"f:app":{}}}}}}`)},
		{Manager: "kubectl-set", Operation: "Update", Time: &t2, FieldsV1: f(`{"f:spec":{"f:template":{"f:spec":{"f:containers":{"k:{\"name\":\"nginx\"}":{"f:image":{}}}}}}}`)},
		{Manager: "kube-controller-manager", Operation: "Update", Subresource: "status", Time: &t3, FieldsV1: f(`{"f:status":{"f:replicas":{}}}`)},
	}
}

func TestAttribute(t *testing.T) {
	obj := []byte(`{"metadata":{"labels":{"app":"api"}}}`)
	for _, tc := range []struct {
		name  string
		c     store.Change
		want  string
		drift bool
	}{
		{"scale has no timestamp", store.Change{Type: store.Modified, Paths: []string{"/spec/replicas"}}, "kubectl", false},
		{"list item by index", store.Change{Type: store.Modified, Paths: []string{"/spec/template/spec/containers/0/image"}}, "kubectl-set", false},
		{"escaped label key", store.Change{Type: store.Modified, Paths: []string{"/metadata/labels/app"}}, "kubectl-create", false},
		{"removed field: newest timed writer", store.Change{Type: store.Modified, Paths: []string{"/spec/paused"}}, "kubectl-set", false},
		{"created", store.Change{Type: store.Added}, "kubectl-set", false},
	} {
		tc.c.Object = obj
		attribute(&tc.c, deploymentManagers())
		if tc.c.Author != tc.want || tc.c.Source != "manual" || tc.c.Drift != tc.drift {
			t.Errorf("%s: author=%q source=%q drift=%v, want %q/manual/%v", tc.name, tc.c.Author, tc.c.Source, tc.c.Drift, tc.want, tc.drift)
		}
	}

	c := store.Change{Type: store.Modified, Paths: []string{"/data/LOG_LEVEL"},
		Object: []byte(`{"metadata":{"labels":{"argocd.argoproj.io/instance":"shop"}}}`)}
	attribute(&c, []metav1.ManagedFieldsEntry{{Manager: "kubectl-patch", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{"f:LOG_LEVEL":{}}}`)}}})
	if c.Author != "kubectl-patch" || !c.Drift {
		t.Errorf("manual edit of Argo CD object: author=%q drift=%v, want kubectl-patch/drift", c.Author, c.Drift)
	}

	c = store.Change{Type: store.Deleted}
	attribute(&c, nil)
	if c.Author != "" || c.Source != "unknown" {
		t.Errorf("deleted: author=%q source=%q", c.Author, c.Source)
	}
}
