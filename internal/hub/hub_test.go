package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/guipguia/mnemo/internal/store"
)

func TestHandler(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Callers sending "X-Test-Namespace: x" may only see namespace x.
	access := func(r *http.Request) store.Access {
		if ns := r.Header.Get("X-Test-Namespace"); ns != "" {
			return store.Access{Scopes: []store.Scope{{Namespaces: []string{ns}}}}
		}
		return store.Access{All: true}
	}
	srv := httptest.NewServer(Handler(s, "secret", access))
	defer srv.Close()

	t0 := time.Date(2026, 10, 20, 14, 30, 0, 0, time.UTC)
	batch := Batch{Cluster: "prod", ClusterUID: "uid-1", Items: []Item{{
		Change: store.Change{
			Ref:  store.Ref{Kind: "ConfigMap", Namespace: "shop", Name: "cfg"},
			Time: t0, Object: json.RawMessage(`{"data":{"a":"1"}}`),
		},
		Managers: []metav1.ManagedFieldsEntry{{Manager: "kubectl-create"}},
	}}, Checkpoints: map[string]string{"v1/configmaps": "42"}}

	post := func(token string, b Batch) int {
		body, _ := json.Marshal(b)
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/ingest", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("wrong", batch); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	if code := post("secret", batch); code != http.StatusOK {
		t.Fatalf("ingest: %d", code)
	}
	batch.ClusterUID = "uid-2"
	if code := post("secret", batch); code != http.StatusConflict {
		t.Fatalf("cluster name reuse: %d, want 409", code)
	}

	get := func(path string, v any) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if v != nil && resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode
	}

	var feed []store.Change
	if code := get("/api/v1/changes?cluster=prod&source=manual", &feed); code != 200 || len(feed) != 1 || feed[0].Type != store.Added {
		t.Fatalf("feed: %d %+v", code, feed)
	}
	var c store.Change
	if code := get("/api/v1/at?cluster=prod&kind=ConfigMap&namespace=shop&name=cfg&time=2026-10-20T14:31:00Z", &c); code != 200 || string(c.Object) != `{"data":{"a":"1"}}` {
		t.Fatalf("at: %d %s", code, c.Object)
	}
	if code := get("/api/v1/at?cluster=prod&kind=ConfigMap&namespace=shop&name=cfg&time=2026-10-20T14:29:00Z", nil); code != 404 {
		t.Fatalf("at before creation: %d, want 404", code)
	}
	if code := get("/api/v1/changes?since=yesterday", nil); code != 400 {
		t.Fatalf("bad time: %d, want 400", code)
	}
	restricted := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("X-Test-Namespace", "other")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, strings.TrimSpace(b.String())
	}
	for path, want := range map[string]string{
		"/api/v1/changes":  "[]",
		"/api/v1/clusters": `[{"name":"prod"`, // cluster visible: namespace "other" may exist there
		"/api/v1/history?cluster=prod&kind=ConfigMap&namespace=shop&name=cfg": "[]",
	} {
		if code, body := restricted(path); code != 200 || !strings.HasPrefix(body, want) {
			t.Errorf("restricted %s: %d %s", path, code, body)
		}
	}
	for _, path := range []string{
		"/api/v1/at?cluster=prod&kind=ConfigMap&namespace=shop&name=cfg",
		fmt.Sprintf("/api/v1/changes/%d", feed[0].ID),
	} {
		if code, _ := restricted(path); code != 404 {
			t.Errorf("restricted %s: %d, want 404 (indistinguishable from missing)", path, code)
		}
	}

	if code := get("/api/v1/checkpoints?cluster=prod&uid=uid-1", nil); code != 401 {
		t.Fatalf("checkpoints without token: %d, want 401", code)
	}

	c1 := NewClient(srv.URL, "secret")
	if cp, err := c1.Resume(t.Context(), "prod", "uid-1"); err != nil || cp["v1/configmaps"] != "42" {
		t.Fatalf("resume: %v, %v", cp, err)
	}
	if _, err := c1.Resume(t.Context(), "prod", "uid-2"); err == nil {
		t.Fatal("resume with another cluster's name must fail")
	}
	batch.ClusterUID = "uid-1"
	batch.Checkpoints = map[string]string{"v1/configmaps": "43"}
	if err := c1.Push(t.Context(), batch); err != nil {
		t.Fatalf("client push: %v", err)
	}
}
