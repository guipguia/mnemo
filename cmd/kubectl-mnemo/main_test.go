package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guipguia/mnemo/internal/hub"
	"github.com/guipguia/mnemo/internal/store"
)

func TestCommands(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Versions 3h and 1h ago, then the "now" side of the diff sees v2.
	now := time.Now()
	for i, obj := range []string{`{"data":{"timeout":"30s"}}`, `{"data":{"timeout":"3s"}}`} {
		c := &store.Change{Ref: store.Ref{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "app"},
			Time: now.Add(-time.Duration(3-2*i) * time.Hour), Author: "kubectl-edit", Source: "manual", Object: json.RawMessage(obj)}
		if _, err := st.Record(ctx, c, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	var gotHeader string
	api := hub.Handler(st, "", func(*http.Request) store.Access { return store.Access{All: true} })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Cookie")
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()

	exec := func(args ...string) (string, error) {
		var out bytes.Buffer
		args = append(args, "--hub", srv.URL, "--cluster", "prod", "--group", "", "-n", "shop", "--header", "Cookie: s=1")
		err := run(ctx, args, &out)
		return out.String(), err
	}

	out, err := exec("changes", "--since", "2h")
	if err != nil || !strings.Contains(out, "shop/ConfigMap/app") || strings.Count(out, "\n") != 2 {
		t.Errorf("changes --since: %v\n%s", err, out)
	}
	if gotHeader != "s=1" {
		t.Errorf("header = %q", gotHeader)
	}
	out, err = exec("history", "ConfigMap/app")
	if err != nil || !strings.Contains(out, "/data/timeout") {
		t.Errorf("history: %v\n%s", err, out)
	}
	out, err = exec("get", "ConfigMap/app", "--at", "2h")
	if err != nil || !strings.Contains(out, "timeout: 30s") {
		t.Errorf("get --at: %v\n%s", err, out)
	}
	out, err = exec("diff", "ConfigMap/app", "--at", "2h")
	if !errors.Is(err, errDiffers) || !strings.Contains(out, "-  timeout: 30s") || !strings.Contains(out, "+  timeout: 3s") {
		t.Errorf("diff: %v\n%s", err, out)
	}
	if out, err = exec("diff", "ConfigMap/app", "--at", "30m"); err != nil || out != "" {
		t.Errorf("no difference: %v\n%s", err, out)
	}
	// Before it existed: added from nothing.
	if out, err = exec("diff", "ConfigMap/app", "--at", "4h"); !errors.Is(err, errDiffers) || !strings.Contains(out, "+  timeout: 3s") {
		t.Errorf("diff before creation: %v\n%s", err, out)
	}
	if _, err = exec("get", "ConfigMap/nope"); err == nil || !strings.Contains(err.Error(), `in cluster "prod"`) {
		t.Errorf("missing: %v", err)
	}
	if _, err = exec("get", "nope"); err == nil || !strings.Contains(err.Error(), "KIND/NAME") {
		t.Errorf("bad arg: %v", err)
	}
	if _, err = exec("get", "ConfigMap/app", "--at", "yesterday"); err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Errorf("bad time: %v", err)
	}
}
