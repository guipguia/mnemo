package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
		"format":    {"webhooks:\n- {name: a, url: \"https://x.example.com\", format: splunk}\n", "format \"splunk\""},
		"no token":  {"webhooks:\n- {name: a, url: \"https://x.example.com\", format: grafana}\n", "needs a token"},
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

func TestDeliverGrafana(t *testing.T) {
	var got annotation
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	t0 := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	c := store.Change{Ref: store.Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "api"},
		Time: t0, Type: store.Modified, Author: "<bob>", Source: "manual", Drift: true}
	w := Webhook{URL: srv.URL, Format: formatGrafana, Token: "glsa"}
	if !w.deliver(context.Background(), srv.Client(), "https://m.example.com", c, slog.Default()) {
		t.Fatal("not delivered")
	}
	if auth != "Bearer glsa" || got.Time != t0.UnixMilli() {
		t.Errorf("auth %q, time %d", auth, got.Time)
	}
	wantTags := []string{"mnemo", "cluster:prod", "kind:ConfigMap", "type:modified", "source:manual", "namespace:shop", "drift"}
	if !slices.Equal(got.Tags, wantTags) {
		t.Errorf("tags = %v", got.Tags)
	}
	// Grafana renders HTML: object data is escaped, and the link is a real one.
	if !strings.Contains(got.Text, "by &lt;bob&gt;") || !strings.Contains(got.Text, `<a href="https://m.example.com/?change=0&amp;cluster=prod`) {
		t.Errorf("text = %q", got.Text)
	}

	// Cluster-scoped: no namespace tag.
	c = store.Change{Ref: store.Ref{Cluster: "prod", Kind: "ClusterRole", Name: "admin"}, Time: t0, Type: store.Added, Source: "gitops"}
	w.deliver(context.Background(), srv.Client(), "", c, slog.Default())
	if !slices.Equal(got.Tags, []string{"mnemo", "cluster:prod", "kind:ClusterRole", "type:added", "source:gitops"}) {
		t.Errorf("cluster-scoped tags = %v", got.Tags)
	}

	// token without format: the mnemo payload, with the bearer token.
	auth = ""
	w = Webhook{URL: srv.URL, Token: "t0k"}
	w.deliver(context.Background(), srv.Client(), "", c, slog.Default())
	if auth != "Bearer t0k" {
		t.Errorf("plain webhook auth = %q", auth)
	}
}

func TestRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid API key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	err := Webhook{URL: srv.URL}.post(context.Background(), srv.Client(), nil, 1)
	if !errors.Is(err, errRejected) {
		t.Errorf("401: err = %v, want errRejected", err)
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

func TestPostErrorHidesURL(t *testing.T) {
	w := Webhook{Name: "slack", URL: "http://127.0.0.1:1/services/T000/B000/SECRETPATH?token=QSECRET"}
	err := w.post(t.Context(), &http.Client{}, []byte(`{}`), 1)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v, want an error without the URL", err)
	}
}
