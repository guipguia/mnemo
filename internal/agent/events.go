package agent

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guipguia/mnemo/internal/store"
)

// Events are archived rather than diffed: the hub keeps them after the API server expires them.
var eventsGVR = schema.GroupVersionResource{Version: "v1", Resource: "events"}

// toEvent reads a core/v1 Event. Its time is the latest of the timestamps the various
// reporters fill in (old ones set lastTimestamp, events.k8s.io ones eventTime and series).
func toEvent(u *unstructured.Unstructured) store.Event {
	str := func(f ...string) string { s, _, _ := unstructured.NestedString(u.Object, f...); return s }
	gv, _ := schema.ParseGroupVersion(str("involvedObject", "apiVersion"))
	e := store.Event{
		Ref: store.Ref{Group: gv.Group, Kind: str("involvedObject", "kind"),
			Namespace: str("involvedObject", "namespace"), Name: str("involvedObject", "name")},
		UID: string(u.GetUID()), Type: str("type"), Reason: str("reason"), Message: str("message"),
		Source: str("reportingComponent"),
	}
	if e.Source == "" {
		e.Source = str("source", "component")
	}
	count, _, _ := unstructured.NestedInt64(u.Object, "count")
	if n, ok, _ := unstructured.NestedInt64(u.Object, "series", "count"); ok {
		count = n
	}
	e.Count = int(max(count, 1))

	created := u.GetCreationTimestamp().Time
	e.FirstTime, e.LastTime = created, created
	for _, f := range [][]string{{"firstTimestamp"}, {"eventTime"}} {
		if t, err := time.Parse(time.RFC3339, str(f...)); err == nil {
			e.FirstTime = t
			break
		}
	}
	for _, f := range [][]string{{"series", "lastObservedTime"}, {"lastTimestamp"}, {"eventTime"}} {
		if t, err := time.Parse(time.RFC3339, str(f...)); err == nil {
			e.LastTime = t
			break
		}
	}
	return e
}
