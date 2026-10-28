package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/fake"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

func event(uid, rv string, fields map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "v1", "kind": "Event",
		"metadata": map[string]any{"name": "web." + uid, "namespace": "shop", "uid": uid, "resourceVersion": rv,
			"creationTimestamp": "2026-10-20T14:00:00Z"},
		"involvedObject": map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "namespace": "shop", "name": "web"},
		"type":           "Normal", "reason": "ScalingReplicaSet", "message": "Scaled up replica set web-7d9f8 to 3",
	}
	for k, v := range fields {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

func TestToEvent(t *testing.T) {
	at := func(s string) time.Time { tm, _ := time.Parse(time.RFC3339, s); return tm }
	for name, tc := range map[string]struct {
		fields      map[string]any
		count       int
		first, last string
		source      string
	}{
		"core": {map[string]any{"count": int64(4), "firstTimestamp": "2026-10-20T14:01:00Z",
			"lastTimestamp": "2026-10-20T14:05:00Z", "source": map[string]any{"component": "deployment-controller"}},
			4, "2026-10-20T14:01:00Z", "2026-10-20T14:05:00Z", "deployment-controller"},
		"events.k8s.io series": {map[string]any{"eventTime": "2026-10-20T14:02:00.123456Z", "reportingComponent": "kubelet",
			"series": map[string]any{"count": int64(7), "lastObservedTime": "2026-10-20T14:09:00.000001Z"}},
			7, "2026-10-20T14:02:00.123456Z", "2026-10-20T14:09:00.000001Z", "kubelet"},
		"no times": {nil, 1, "2026-10-20T14:00:00Z", "2026-10-20T14:00:00Z", ""},
	} {
		e := toEvent(event("1", "1", tc.fields))
		if e.Group != "apps" || e.Kind != "Deployment" || e.Name != "web" || e.UID != "1" ||
			e.Count != tc.count || e.Source != tc.source || !e.FirstTime.Equal(at(tc.first)) || !e.LastTime.Equal(at(tc.last)) {
			t.Errorf("%s: %+v", name, e)
		}
	}
}

func TestArchivesEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{eventsGVR: "EventList"}, event("1", "10", nil))
	a := &Agent{Client: client, Cluster: "test", uid: "kube-system-uid",
		Push: func(ctx context.Context, b hub.Batch) error { return hub.Ingest(ctx, st, b) }}

	if _, err := a.relist(ctx, eventsGVR); err != nil {
		t.Fatal(err)
	}
	w := watch.NewFake()
	go a.consume(ctx, eventsGVR, w, "10")
	w.Add(event("2", "11", map[string]any{"type": "Warning", "reason": "FailedCreate"}))
	w.Delete(event("1", "12", nil)) // expired by the API server: still archived

	f := store.EventFilter{Access: store.Access{All: true}, Cluster: "test", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web"}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		evs, err := st.Events(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		cps, _ := st.Checkpoints(ctx, "test")
		if len(evs) == 2 && cps["v1/events"] == "12" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %+v, checkpoints = %v", evs, cps)
		}
	}
}
