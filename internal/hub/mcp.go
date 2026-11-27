package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guipguia/mnemo/internal/store"
)

// MCPHandler serves the history to AI agents over MCP (streamable HTTP, stateless): the same
// reads as the API, limited by the same access(r). Each request gets a server bound to its
// caller's access, so a tool can never see more than the caller could through the API.
func MCPHandler(s *store.Store, access func(*http.Request) store.Access, version string) http.Handler {
	cache := mcp.NewSchemaCache()
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return newMCPServer(s, access(r), version, cache)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	// Login can be a cookie: refuse cross-origin browser requests outright.
	return http.NewCrossOriginProtection().Handler(h)
}

const timeHelp = "RFC 3339 time (2026-10-20T14:32:00Z) or a duration ago (2h, 90m)"

type changesIn struct {
	Cluster   string `json:"cluster,omitempty" jsonschema:"cluster name; empty: all"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace; empty: all"`
	Kind      string `json:"kind,omitempty" jsonschema:"exact kind, e.g. ConfigMap"`
	Author    string `json:"author,omitempty" jsonschema:"exact author: a user or a field manager such as kubectl-edit"`
	Source    string `json:"source,omitempty" jsonschema:"gitops, controller, manual, or unknown"`
	DriftOnly bool   `json:"driftOnly,omitempty" jsonschema:"only manual changes to GitOps-managed objects"`
	Since     string `json:"since,omitempty" jsonschema:"from this time: an RFC 3339 time or a duration ago such as 30m"`
	Until     string `json:"until,omitempty" jsonschema:"up to this time: an RFC 3339 time or a duration ago"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum changes (default 100, at most 1000)"`
}

type objectIn struct {
	Cluster   string `json:"cluster" jsonschema:"cluster name"`
	Group     string `json:"group,omitempty" jsonschema:"API group: empty for core kinds (ConfigMap), apps for Deployment"`
	Kind      string `json:"kind" jsonschema:"kind, e.g. Deployment"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace; empty for cluster-scoped objects"`
	Name      string `json:"name" jsonschema:"object name"`
}

type historyIn struct {
	objectIn
	Limit int `json:"limit,omitempty" jsonschema:"maximum versions, newest first (default 100)"`
}

type atIn struct {
	objectIn
	Time string `json:"time,omitempty" jsonschema:"RFC 3339 time or a duration ago; empty: now"`
}

type eventsIn struct {
	Cluster   string `json:"cluster,omitempty" jsonschema:"cluster name; empty: all"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace; empty: all"`
	Group     string `json:"group,omitempty" jsonschema:"API group of the object"`
	Kind      string `json:"kind,omitempty" jsonschema:"kind of the object; with name, also its rollout (a Deployment's ReplicaSets and Pods)"`
	Name      string `json:"name,omitempty" jsonschema:"object name"`
	Type      string `json:"type,omitempty" jsonschema:"Normal or Warning"`
	Since     string `json:"since,omitempty" jsonschema:"from this time: an RFC 3339 time or a duration ago"`
	Until     string `json:"until,omitempty" jsonschema:"up to this time: an RFC 3339 time or a duration ago"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum events (default 100, at most 1000)"`
}

func newMCPServer(s *store.Store, a store.Access, version string, cache *mcp.SchemaCache) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mnemo", Version: version}, &mcp.ServerOptions{
		SchemaCache: cache,
		Instructions: "mnemo records every version of Kubernetes objects across clusters, with who changed them. " +
			"To explain an incident, start with list_changes over the window before it, then object_history and " +
			"object_at for the objects involved. Times are " + timeHelp + ".",
	})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	tool := func(name, desc string) *mcp.Tool { return &mcp.Tool{Name: name, Description: desc, Annotations: ro} }

	mcp.AddTool(srv, tool("list_clusters", "Clusters mnemo records, with when each agent last checked in."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			all, err := s.Clusters(ctx)
			visible := []store.Cluster{}
			for _, c := range all {
				if a.AllowsCluster(c.Name) {
					visible = append(visible, c)
				}
			}
			return nil, map[string]any{"clusters": visible}, err
		})

	mcp.AddTool(srv, tool("list_changes", "The change feed, newest first: what changed, when, who did it (author), "+
		"how (source: gitops, controller, manual), whether it was drift, and which fields changed (paths). No object bodies."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in changesIn) (*mcp.CallToolResult, any, error) {
			f := store.Filter{Access: a, Cluster: in.Cluster, Namespace: in.Namespace, Kind: in.Kind,
				Author: in.Author, Source: in.Source, DriftOnly: in.DriftOnly, Limit: in.Limit}
			var err error
			if f.Since, err = when(in.Since); err != nil {
				return nil, nil, err
			}
			if f.Until, err = when(in.Until); err != nil {
				return nil, nil, err
			}
			cs, err := s.Changes(ctx, f)
			return nil, map[string]any{"changes": cs}, err
		})

	mcp.AddTool(srv, tool("object_history", "Every recorded version of one object, newest first, with author, source and changed paths."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, any, error) {
			cs := []store.Change{}
			var err error
			if r := in.ref(); a.Allows(r.Cluster, r.Namespace) {
				cs, err = s.History(ctx, r, in.Limit)
			}
			return nil, map[string]any{"versions": cs}, err
		})

	mcp.AddTool(srv, tool("object_at", "One object exactly as it was at a time (default: now), as normalized JSON. "+
		"Compare two times to see what changed. Secrets are never recorded."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in atIn) (*mcp.CallToolResult, any, error) {
			t, err := when(in.Time)
			if err != nil {
				return nil, nil, err
			}
			if t.IsZero() {
				t = time.Now()
			}
			r := in.ref()
			missing := fmt.Errorf("no %s %s/%s in cluster %q at %s", r.Kind, r.Namespace, r.Name, r.Cluster, t.Format(time.RFC3339))
			if !a.Allows(r.Cluster, r.Namespace) { // first, so errors can't tell hidden from missing
				return nil, nil, missing
			}
			c, err := s.At(ctx, r, t)
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil, missing
			}
			return nil, c, err
		})

	mcp.AddTool(srv, tool("list_events", "Archived Kubernetes Events, most recent first, kept past their usual one-hour lifetime."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in eventsIn) (*mcp.CallToolResult, any, error) {
			f := store.EventFilter{Access: a, Cluster: in.Cluster, Group: in.Group, Kind: in.Kind,
				Namespace: in.Namespace, Name: in.Name, Type: in.Type, Limit: in.Limit}
			var err error
			if f.Since, err = when(in.Since); err != nil {
				return nil, nil, err
			}
			if f.Until, err = when(in.Until); err != nil {
				return nil, nil, err
			}
			evs, err := s.Events(ctx, f)
			return nil, map[string]any{"events": evs}, err
		})
	return srv
}

func (o objectIn) ref() store.Ref {
	return store.Ref{Cluster: o.Cluster, Group: o.Group, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name}
}

// when parses an RFC 3339 time or a duration ago; empty is the zero time.
func when(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return t, fmt.Errorf("time %q: want an %s", s, timeHelp)
	}
	return t, nil
}
