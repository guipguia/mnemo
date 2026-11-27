package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/ingest", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(traced(mux))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/ingest", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
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
				Spans []struct {
					TraceID, SpanID, ParentSpanID, Name, StartTimeUnixNano string
					Kind                                                   int
				}
			}
		}
	}
	if err := json.Unmarshal(<-got, &body); err != nil {
		t.Fatal(err)
	}
	span := body.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if span.TraceID != "0af7651916cd43dd8448eb211c80319c" || span.ParentSpanID != "b7ad6b7169203331" ||
		span.Name != "POST /api/v1/ingest" || span.Kind != 2 || len(span.SpanID) != 16 || span.StartTimeUnixNano == "" {
		t.Errorf("span = %+v", span)
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
	h, err := otlpHeaders("a=1, b = x%20y ,,a=2")
	if err != nil || h.Get("a") != "2" || h.Get("b") != "x y" {
		t.Errorf("headers = %v, %v", h, err)
	}
	if _, err := otlpHeaders("token-without-key"); err == nil {
		t.Errorf("want an error")
	}
	if _, err := otlpHeaders("api-key=%zz-secret"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v: want an error that doesn't echo the value", err)
	}
}
