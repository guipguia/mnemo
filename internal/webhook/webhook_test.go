package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/guipguia/mnemo/internal/store"
)

func TestLoad(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, err string }{
		"ok":        {"url: https://mnemo.example.com\nwebhooks:\n- name: a\n  url: https://x.example.com/hook\n  delay: 35s\n  match: {clusters: [prod], namespaces: [\"shop-*\"], types: [deleted]}\n", ""},
		"typo":      {"webhooks:\n- name: a\n  url: https://x.example.com\n  match: {kind: [ConfigMap]}\n", "unknown field"},
		"duplicate": {"webhooks:\n- {name: a, url: \"https://x.example.com\"}\n- {name: a, url: \"https://y.example.com\"}\n", "unique"},
		"no scheme": {"webhooks:\n- {name: a, url: x.example.com}\n", "url must be"},
		"bad type":  {"webhooks:\n- {name: a, url: \"https://x.example.com\", match: {types: [baseline]}}\n", "type \"baseline\""},
		"pattern":   {"webhooks:\n- {name: a, url: \"https://x.example.com\", match: {clusters: [\"p*d\"]}}\n", "pattern"},
	} {
		p := filepath.Join(t.TempDir(), "webhooks.yaml")
		os.WriteFile(p, []byte(tc.yaml), 0o600)
		c, err := Load(p)
		if tc.err == "" && (err != nil || c.Webhooks[0].Delay.Duration != 35*time.Second) {
			t.Errorf("%s: %+v, %v", name, c, err)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.err)
		}
	}
}

func TestDeliver(t *testing.T) {
	poll = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	record := func(kind, name, obj string, listed bool) {
		t.Helper()
		c := &store.Change{Ref: store.Ref{Cluster: "prod", Group: "rbac.authorization.k8s.io", Kind: kind, Name: name},
			Time: t0, Author: "alice", Source: "manual"}
		if obj != "" {
			c.Object = json.RawMessage(obj)
		}
		if _, err := st.Record(ctx, c, listed, nil); err != nil {
			t.Fatal(err)
		}
	}
	record("ClusterRoleBinding", "before", `{"v":1}`, false) // recorded before the webhook existed

	var mu sync.Mutex
	var got []Payload
	fails := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 { // the receiver is down once: the same change is retried
			fails--
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var p Payload
		json.Unmarshal(body, &p)
		if r.Header.Get("X-Mnemo-Signature") != "sha256="+Sign("s3cret", body) || r.Header.Get("X-Mnemo-Change") == "" {
			t.Errorf("bad headers: %v", r.Header)
		}
		got = append(got, p)
	}))
	defer srv.Close()

	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }() // before st.Close
	w := Webhook{Name: "rbac", URL: srv.URL, Secret: "s3cret", Match: Match{Kinds: []string{"ClusterRoleBinding"}}}
	c := &Config{URL: "https://mnemo.example.com/", Webhooks: []Webhook{w}}
	wg.Go(func() { Run(ctx, st, c) })
	waitFor(t, func() bool { _, ok, _ := st.WebhookCursor(ctx, "rbac"); return ok })

	record("ClusterRoleBinding", "baseline", `{"v":1}`, true) // not a change: skipped
	record("ClusterRole", "other", `{"v":1}`, false)          // another kind: skipped
	record("ClusterRoleBinding", "admin", `{"v":1}`, false)
	record("ClusterRoleBinding", "admin", `{"v":2}`, false)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })

	mu.Lock()
	if got[0].Name != "admin" || got[0].Type != store.Added || got[1].Type != store.Modified || got[0].ID >= got[1].ID {
		t.Errorf("delivered %+v", got)
	}
	if want := "prod: ClusterRoleBinding admin added by alice (manual) https://mnemo.example.com/?change="; !strings.HasPrefix(got[0].Text, want) {
		t.Errorf("text = %q, want prefix %q", got[0].Text, want)
	}
	mu.Unlock()
	last, _ := st.LastID(ctx)
	waitFor(t, func() bool { id, _, _ := st.WebhookCursor(ctx, "rbac"); return id == last })

	// Delay holds changes back.
	w.Name, w.Delay, w.Match = "slow", metav1.Duration{Duration: 300 * time.Millisecond}, Match{}
	slow := &Config{Webhooks: []Webhook{w}}
	wg.Go(func() { Run(ctx, st, slow) })
	waitFor(t, func() bool { _, ok, _ := st.WebhookCursor(ctx, "slow"); return ok })
	start := time.Now()
	record("ClusterRoleBinding", "admin", `{"v":3}`, false)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 4 }) // "rbac" and "slow"
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("delayed webhook delivered after %v", d)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}
