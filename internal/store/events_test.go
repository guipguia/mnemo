package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestEvents(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	t0 := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	ev := func(uid, group, kind, ns, name string, m int) Event {
		at := t0.Add(time.Duration(m) * time.Minute)
		return Event{Ref: Ref{Group: group, Kind: kind, Namespace: ns, Name: name}, UID: uid,
			Type: "Normal", Reason: "R", Count: 1, FirstTime: at, LastTime: at}
	}
	if err := s.RecordEvents(ctx, "prod", []Event{
		ev("1", "apps", "Deployment", "shop", "web", 0),
		ev("2", "apps", "ReplicaSet", "shop", "web-7d9f8c4b5", 1),
		ev("3", "", "Pod", "shop", "web-7d9f8c4b5-x2k9z", 2),
		ev("4", "apps", "Deployment", "shop", "web-api", 3),      // another Deployment
		ev("5", "", "Pod", "shop", "web-api-7d9f8c4b5-x2k9z", 4), // its Pod
		ev("6", "", "Pod", "other", "web-7d9f8c4b5-x2k9z", 5),    // another namespace
		ev("7", "", "ConfigMap", "shop", "web", 6),               // same name, another kind
	}); err != nil {
		t.Fatal(err)
	}
	// The API server bumps an event's count instead of creating a new one.
	bumped := ev("3", "", "Pod", "shop", "web-7d9f8c4b5-x2k9z", 9)
	bumped.Count = 4
	if err := s.RecordEvents(ctx, "prod", []Event{bumped}); err != nil {
		t.Fatal(err)
	}

	uids := func(f EventFilter) []string {
		t.Helper()
		evs, err := s.Events(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range evs {
			out = append(out, e.UID)
		}
		return out
	}
	all := Access{All: true}
	web := EventFilter{Access: all, Cluster: "prod", Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web"}
	if got, want := uids(web), []string{"3", "2", "1"}; !slices.Equal(got, want) {
		t.Fatalf("rollout of web = %v, want %v", got, want)
	}
	web.Limit = 1
	if got := uids(web); !slices.Equal(got, []string{"3"}) {
		t.Fatalf("limit 1 = %v", got)
	}
	cm := EventFilter{Access: all, Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "web"}
	if got := uids(cm); !slices.Equal(got, []string{"7"}) {
		t.Fatalf("configmap web = %v", got)
	}
	if got := uids(EventFilter{Access: Access{Scopes: []Scope{{Namespaces: []string{"other"}}}}}); !slices.Equal(got, []string{"6"}) {
		t.Fatalf("namespace other only = %v", got)
	}
	if got := uids(EventFilter{}); len(got) != 0 {
		t.Fatalf("zero access = %v", got)
	}

	evs, _ := s.Events(ctx, EventFilter{Access: all, Kind: "Pod", Namespace: "shop", Name: "web-7d9f8c4b5-x2k9z"})
	if len(evs) != 1 || evs[0].Count != 4 || !evs[0].FirstTime.Equal(t0.Add(2*time.Minute)) || !evs[0].LastTime.Equal(t0.Add(9*time.Minute)) {
		t.Fatalf("bumped event = %+v", evs)
	}

	if n, err := s.ExpireEvents(ctx, t0.Add(5*time.Minute)); err != nil || n != 4 {
		t.Fatalf("expire = %d, %v; want 4 (event 3 was bumped)", n, err)
	}
}
