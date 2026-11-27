package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guipguia/mnemo/internal/auth"
	"github.com/guipguia/mnemo/internal/store"
)

// TestTracing checks the OTLP/JSON a collector receives for one traced request that came
// with a W3C traceparent (as the agent's pushes do).
func TestTracing(t *testing.T) {
	got := make(chan []byte, 10)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer a=b" {
			t.Errorf("collector got %s %s, headers %v", r.Method, r.URL, r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		got <- b
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL+"/")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer%20a%3Db")
	t.Setenv("OTEL_SERVICE_NAME", "mnemo-test")
	shutdown, err := tracing(context.Background(), "hub")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tracingOn = false })

	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	none, err := auth.New(context.Background(), auth.Config{Mode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(traced(routes(st, "tok", none)))
	defer srv.Close()
	const parent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	for _, path := range []string{"/api/v1/changes", "/api/v1/history?cluster=c&kind=ConfigMap&name=x"} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("traceparent", parent) // from anyone: a link, not the parent
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/ingest", strings.NewReader(`{"cluster":"c","clusterUID":"u"}`))
	req.Header.Set("traceparent", parent) // an agent push: continues its trace
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	type span struct {
		TraceID, SpanID, ParentSpanID, Name, StartTimeUnixNano string
		Kind                                                   int
		Links                                                  []struct{ TraceID string }
	}
	var body struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []struct {
					Key   string
					Value map[string]any
				}
			}
			ScopeSpans []struct {
				Spans []span
			}
		}
	}
	if err := json.Unmarshal(<-got, &body); err != nil {
		t.Fatal(err)
	}
	spans := map[string]span{}
	for _, sc := range body.ResourceSpans[0].ScopeSpans {
		for _, sp := range sc.Spans {
			spans[sp.Name] = sp
		}
	}
	for _, name := range []string{"GET /api/v1/changes", "GET /api/v1/history"} {
		sp, ok := spans[name]
		if !ok || sp.TraceID == "0af7651916cd43dd8448eb211c80319c" || len(sp.Links) != 1 || sp.Kind != 2 {
			t.Errorf("span %q = %+v (found %v), want a new trace linking the caller's", name, sp, ok)
		}
	}
	push := spans["POST /api/v1/ingest"]
	if push.TraceID != "0af7651916cd43dd8448eb211c80319c" || push.ParentSpanID != "b7ad6b7169203331" || len(push.SpanID) != 16 || push.StartTimeUnixNano == "" {
		t.Errorf("ingest span = %+v, want the agent's trace continued", push)
	}
	if in := spans["ingest"]; in.ParentSpanID != push.SpanID {
		t.Errorf("ingest transaction span = %+v, want a child of the request", in)
	}
	attrs := map[string]any{}
	for _, a := range body.ResourceSpans[0].Resource.Attributes {
		attrs[a.Key] = a.Value["stringValue"]
	}
	if attrs["service.name"] != "mnemo-test" || attrs["mnemo.mode"] != "hub" {
		t.Errorf("resource = %v", attrs)
	}
}

func TestOTLPHeaders(t *testing.T) {
	h := http.Header{}
	if err := otlpHeaders(h, "a=1, b = x%20y ,,a=2,auth=Basic dXNlcj+wYXNz"); err != nil || h.Get("a") != "2" || h.Get("b") != "x y" || h.Get("auth") != "Basic dXNlcj+wYXNz" {
		t.Errorf("headers = %v, %v", h, err)
	}
	for _, bad := range []string{"Bearer secret-token", "api-key=%zz-secret"} {
		if err := otlpHeaders(h, bad); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: err = %v, want an error that doesn't echo the entry", bad, err)
		}
	}
}
