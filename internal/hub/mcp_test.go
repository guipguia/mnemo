package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guipguia/mnemo/internal/store"
)

func TestMCP(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	for i, rf := range []store.Ref{
		{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "app"},
		{Cluster: "prod", Kind: "ConfigMap", Namespace: "shop", Name: "app"},
		{Cluster: "prod", Kind: "ConfigMap", Namespace: "secret-team", Name: "hidden"},
	} {
		c := &store.Change{Ref: rf, Time: now.Add(time.Duration(i-3) * time.Hour), Author: "kubectl-edit", Source: "manual",
			Object: json.RawMessage(`{"data":{"timeout":"` + []string{"30s", "3s", "1s"}[i] + `"}}`)}
		if _, err := st.Record(ctx, c, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	st.RegisterCluster(ctx, "prod", "u")
	// The caller may only see namespace shop.
	access := func(*http.Request) store.Access {
		return store.Access{Scopes: []store.Scope{{Namespaces: []string{"shop"}}}}
	}
	srv := httptest.NewServer(MCPHandler(st, access, "test"))
	defer srv.Close()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 5 {
		t.Fatalf("tools: %v, %v", tools, err)
	}
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res.Content[0].(*mcp.TextContent).Text, res.IsError
	}

	if out, _ := call("list_changes", map[string]any{"since": "2h30m"}); !strings.Contains(out, `"author":"kubectl-edit"`) || strings.Count(out, `"id"`) != 1 {
		t.Errorf("list_changes since: %s", out)
	}
	if out, _ := call("list_changes", nil); strings.Contains(out, "hidden") || strings.Count(out, `"id"`) != 2 {
		t.Errorf("list_changes must hide other namespaces: %s", out)
	}
	app := map[string]any{"cluster": "prod", "kind": "ConfigMap", "namespace": "shop", "name": "app"} // no group: core
	if out, _ := call("object_history", app); strings.Count(out, `"id"`) != 2 || !strings.Contains(out, "/data/timeout") {
		t.Errorf("object_history: %s", out)
	}
	app["time"] = "2h30m"
	if out, isErr := call("object_at", app); isErr || !strings.Contains(out, `"timeout":"30s"`) {
		t.Errorf("object_at: %s", out)
	}
	hidden := map[string]any{"cluster": "prod", "group": "", "kind": "ConfigMap", "namespace": "secret-team", "name": "hidden"}
	if out, isErr := call("object_at", hidden); !isErr || strings.Contains(out, "1s") {
		t.Errorf("object_at must hide other namespaces: %s", out)
	}
	if out, _ := call("object_history", hidden); strings.Count(out, `"id"`) != 0 {
		t.Errorf("object_history must hide other namespaces: %s", out)
	}
	if out, isErr := call("list_changes", map[string]any{"since": "yesterday"}); !isErr || !strings.Contains(out, "RFC 3339") {
		t.Errorf("bad time: %s", out)
	}
	// A browser on another site can't post to it, even with the user's cookie.
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST: %v, %v", resp.Status, err)
	}
	if out, _ := call("list_clusters", nil); !strings.Contains(out, `"name":"prod"`) {
		t.Errorf("list_clusters: %s", out)
	}
	if out, isErr := call("list_events", map[string]any{"cluster": "prod"}); isErr || !strings.Contains(out, `"events"`) {
		t.Errorf("list_events: %s", out)
	}
}
