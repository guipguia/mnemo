package agent

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

var configmaps = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// harness runs a real agent and an in-process hub (real SQLite) against a fake API server
// whose lists and watch events the test hands out one at a time.
type harness struct {
	t       *testing.T
	st      *store.Store
	agent   *Agent
	lists   chan *unstructured.UnstructuredList // each List call takes the next one
	watches chan watchCall                      // each Watch call is handed to the test
	listN   atomic.Int32
}

type watchCall struct {
	rv string
	w  *watch.FakeWatcher
}

func newHarness(t *testing.T) *harness {
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "kube-system", "uid": "kube-system-uid"},
	}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{configmaps: "ConfigMapList"}, ns)

	h := &harness{t: t, st: st, lists: make(chan *unstructured.UnstructuredList, 4), watches: make(chan watchCall)}
	client.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		h.listN.Add(1)
		select {
		case l := <-h.lists:
			return true, l, nil
		case <-time.After(5 * time.Second):
			return true, nil, errors.New("test gave no list")
		}
	})
	client.PrependWatchReactor("configmaps", func(a clienttesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFake()
		select {
		case h.watches <- watchCall{a.(clienttesting.WatchActionImpl).GetWatchRestrictions().ResourceVersion, w}:
		case <-time.After(5 * time.Second): // test is over
		}
		return true, w, nil
	})

	h.agent = &Agent{
		Client: client, Cluster: "test", Resources: []schema.GroupVersionResource{configmaps},
		Push: func(ctx context.Context, b hub.Batch) error { return hub.Ingest(ctx, st, b) },
		Resume: func(ctx context.Context, c, uid string) (map[string]string, error) {
			return hub.Resume(ctx, st, c, uid)
		},
	}
	return h
}

func (h *harness) run() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.agent.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			h.t.Fatal("agent did not stop")
		}
	}
}

func (h *harness) nextWatch(wantRV string) *watch.FakeWatcher {
	h.t.Helper()
	select {
	case c := <-h.watches:
		if c.rv != wantRV {
			h.t.Fatalf("watch from resourceVersion %q, want %q", c.rv, wantRV)
		}
		return c.w
	case <-time.After(5 * time.Second):
		h.t.Fatalf("agent never watched (wanted resourceVersion %q)", wantRV)
		return nil
	}
}

// eventually polls until check returns "" (success) or 5s pass.
func (h *harness) eventually(what string, check func() string) {
	h.t.Helper()
	var last string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if last = check(); last == "" {
			return
		}
	}
	h.t.Fatalf("%s: %s", what, last)
}

func (h *harness) checkpoint(want string) {
	h.t.Helper()
	h.eventually("checkpoint", func() string {
		cp, _ := h.st.Checkpoints(context.Background(), "test")
		if cp["v1/configmaps"] != want {
			return "checkpoint is " + cp["v1/configmaps"] + ", want " + want
		}
		return ""
	})
}

// history returns "type:author" per version of a ConfigMap, newest first.
func (h *harness) history(name string) []string {
	cs, _ := h.st.History(context.Background(), store.Ref{Cluster: "test", Kind: "ConfigMap", Namespace: "ns", Name: name}, 0)
	var out []string
	for _, c := range cs {
		out = append(out, string(c.Type)+":"+c.Author)
	}
	return out
}

func (h *harness) wantHistory(name string, want ...string) {
	h.t.Helper()
	h.eventually("history of "+name, func() string {
		if got := h.history(name); !slices.Equal(got, want) {
			return "got " + joinOrNone(got) + ", want " + joinOrNone(want)
		}
		return ""
	})
}

func joinOrNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	out := s[0]
	for _, x := range s[1:] {
		out += ", " + x
	}
	return out
}

// cm builds a ConfigMap whose managedFields say manager owns every data key.
func cm(name, rv, manager string, data map[string]any) *unstructured.Unstructured {
	fields := map[string]any{}
	for k := range data {
		fields["f:"+k] = map[string]any{}
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "ns", "uid": name + "-uid", "resourceVersion": rv},
		"data":     data,
	}}
	t := metav1.NewTime(time.Now().Add(-time.Second))
	fv, _ := (&unstructured.Unstructured{Object: map[string]any{"f:data": fields}}).MarshalJSON()
	u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: manager, Operation: "Update", Time: &t, FieldsV1: &metav1.FieldsV1{Raw: fv}}})
	return u
}

func list(rv string, items ...*unstructured.Unstructured) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMapList"}}
	l.SetResourceVersion(rv)
	for _, i := range items {
		l.Items = append(l.Items, *i)
	}
	return l
}

func TestWatchLoop(t *testing.T) {
	h := newHarness(t)

	// First start: no checkpoint, so the agent lists, records a baseline, and watches from the list.
	h.lists <- list("100", cm("a", "90", "argocd-controller", map[string]any{"K": "1"}))
	stop := h.run()
	defer stop()
	w := h.nextWatch("100")
	h.wantHistory("a", "baseline:argocd-controller")
	h.checkpoint("100")

	// A watch event: attributed from managedFields, checkpoint advances.
	w.Modify(cm("a", "101", "kubectl-patch", map[string]any{"K": "2"}))
	h.wantHistory("a", "modified:kubectl-patch", "baseline:argocd-controller")
	h.checkpoint("101")

	// A bookmark only advances the checkpoint.
	w.Action(watch.Bookmark, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"resourceVersion": "150"}}})
	h.checkpoint("150")

	// The resume point expired (410): relist. "a" was deleted and "b" created meanwhile.
	h.lists <- list("200", cm("b", "190", "kubectl-create", map[string]any{"X": "1"}))
	w.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired})
	w = h.nextWatch("200")
	h.wantHistory("a", "deleted:", "modified:kubectl-patch", "baseline:argocd-controller")
	h.wantHistory("b", "baseline:kubectl-create")
	h.checkpoint("200")

	// A delete event leaves a tombstone.
	w.Delete(cm("b", "201", "kubectl-create", map[string]any{"X": "1"}))
	h.wantHistory("b", "deleted:", "baseline:kubectl-create")

	// The server closing a watch normally is not an error: rewatch from the last
	// resourceVersion, without relisting.
	lists := h.listN.Load()
	w.Stop()
	h.nextWatch("201")
	if n := h.listN.Load(); n != lists {
		t.Errorf("relisted after a normal watch close (%d lists, want %d)", n, lists)
	}
}

func TestResumesFromHubCheckpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := hub.Resume(ctx, h.st, "test", "kube-system-uid"); err != nil {
		t.Fatal(err)
	}
	h.st.SetCheckpoint(ctx, "test", "v1/configmaps", "500")

	// A restarted agent keeps no state: it resumes from the hub's checkpoint, no list.
	stop := h.run()
	defer stop()
	h.nextWatch("500")
	if n := h.listN.Load(); n != 0 {
		t.Errorf("listed %d times; a resume point existed", n)
	}
}

func TestRefusesClusterNameOfAnotherCluster(t *testing.T) {
	h := newHarness(t)
	if _, err := hub.Resume(context.Background(), h.st, "test", "some-other-cluster-uid"); err != nil {
		t.Fatal(err)
	}
	h.agent.Resume = func(ctx context.Context, c, uid string) (map[string]string, error) {
		return hub.Resume(ctx, h.st, c, uid)
	}
	stop := h.run()
	defer stop()
	// The agent must keep retrying instead of watching with someone else's checkpoints.
	select {
	case c := <-h.watches:
		t.Fatalf("watched from %q with a cluster name owned by another cluster", c.rv)
	case <-time.After(1500 * time.Millisecond):
	}
}

func TestPushRetriesUntilHubAccepts(t *testing.T) {
	h := newHarness(t)
	var calls atomic.Int32
	ingest := h.agent.Push
	h.agent.Push = func(ctx context.Context, b hub.Batch) error {
		if calls.Add(1) <= 2 {
			return errors.New("hub unavailable")
		}
		return ingest(ctx, b)
	}
	h.lists <- list("100", cm("a", "90", "kubectl-create", map[string]any{"K": "1"}))
	stop := h.run()
	defer stop()
	h.nextWatch("100") // only after the baseline push finally succeeded
	h.wantHistory("a", "baseline:kubectl-create")
	if n := calls.Load(); n != 3 {
		t.Errorf("push attempts = %d, want 3 (two failures, then success)", n)
	}
}

func TestParseResource(t *testing.T) {
	for in, want := range map[string]string{
		"v1/configmaps":       "/v1/configmaps",
		"apps/v1/deployments": "apps/v1/deployments",
	} {
		gvr, err := ParseResource(in)
		if err != nil || gvr.Group+"/"+gvr.Version+"/"+gvr.Resource != want {
			t.Errorf("ParseResource(%q) = %v, %v", in, gvr, err)
		}
		if key(gvr) != in {
			t.Errorf("key(%v) = %q, want %q (checkpoints are stored under it)", gvr, key(gvr), in)
		}
	}
	for _, bad := range []string{"configmaps", "a/b/c/d", "v1/secrets"} {
		if _, err := ParseResource(bad); err == nil {
			t.Errorf("ParseResource(%q) accepted", bad)
		}
	}
}
