package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// tracing exports OpenTelemetry traces over OTLP/HTTP when the standard
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT is set; otherwise it's
// a no-op. The SDK reads OTEL_TRACES_SAMPLER, OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES.
func tracing(ctx context.Context, mode string) (shutdown func(context.Context) error, err error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if base := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint == "" && base != "" {
		endpoint = strings.TrimSuffix(base, "/") + "/v1/traces"
	}
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	headers, err := otlpHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS") + "," + os.Getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS"))
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx, resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName("mnemo"), semconv.ServiceVersion(version), attribute.String("mnemo.mode", mode)),
		resource.WithFromEnv()) // last: OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES win
	if err != nil {
		return nil, err
	}
	exp := &otlpExporter{url: endpoint, headers: headers, client: &http.Client{Timeout: 10 * time.Second}}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { slog.Warn("tracing", "err", err) }))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
}

// traced makes a server span per request, named by its route.
func traced(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "mnemo", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
		if r.Pattern != "" {
			return r.Pattern
		}
		return r.Method
	}))
}

// otlpHeaders parses "k=v,k2=v2" with URL-encoded values, as OTEL_EXPORTER_OTLP_HEADERS.
// Later entries win, so the traces-specific variable goes last.
func otlpHeaders(s string) (http.Header, error) {
	h := http.Header{}
	for entry := range strings.SplitSeq(s, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		k, v, ok := strings.Cut(entry, "=")
		k = strings.TrimSpace(k)
		val, err := url.QueryUnescape(strings.TrimSpace(v))
		if !ok || err != nil || k == "" {
			return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_HEADERS: bad entry for %q: want key=value", k) // never echo the value: it's often a token
		}
		h.Set(k, val)
	}
	return h, nil
}

// otlpExporter posts spans as OTLP/HTTP JSON, which every OTLP receiver accepts. The
// official exporter speaks protobuf and links gRPC, which would add 11 MB to the binary.
// ponytail: no retries; the batcher drops a batch the collector refuses.
type otlpExporter struct {
	url     string
	headers http.Header
	client  *http.Client
}

func (e *otlpExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	body, err := json.Marshal(otlpTraces(spans))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = e.headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("OTLP export to %s: %s: %s", e.url, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

func (e *otlpExporter) Shutdown(context.Context) error { return nil }

type kv = map[string]any

// otlpTraces renders spans as an OTLP ExportTraceServiceRequest in the protobuf JSON
// mapping: hex IDs, 64-bit integers as strings. One resource: a process has one.
func otlpTraces(spans []sdktrace.ReadOnlySpan) kv {
	if len(spans) == 0 {
		return kv{"resourceSpans": []kv{}}
	}
	type scope struct{ name, version string }
	var order []scope
	byScope := map[scope][]kv{}
	for _, s := range spans {
		sc := scope{s.InstrumentationScope().Name, s.InstrumentationScope().Version}
		if _, ok := byScope[sc]; !ok {
			order = append(order, sc)
		}
		span := kv{
			"traceId":           s.SpanContext().TraceID().String(),
			"spanId":            s.SpanContext().SpanID().String(),
			"name":              s.Name(),
			"kind":              int(s.SpanKind()), // same numbering as OTLP's SpanKind
			"startTimeUnixNano": nanos(s.StartTime()),
			"endTimeUnixNano":   nanos(s.EndTime()),
			"attributes":        otlpAttrs(s.Attributes()),
			"status":            kv{"code": otlpStatus(s.Status().Code), "message": s.Status().Description},
		}
		if p := s.Parent(); p.IsValid() {
			span["parentSpanId"] = p.SpanID().String()
		}
		var events []kv
		for _, ev := range s.Events() {
			events = append(events, kv{"timeUnixNano": nanos(ev.Time), "name": ev.Name, "attributes": otlpAttrs(ev.Attributes)})
		}
		if events != nil {
			span["events"] = events
		}
		byScope[sc] = append(byScope[sc], span)
	}
	var scopeSpans []kv
	for _, sc := range order {
		scopeSpans = append(scopeSpans, kv{"scope": kv{"name": sc.name, "version": sc.version}, "spans": byScope[sc]})
	}
	return kv{"resourceSpans": []kv{{
		"resource":   kv{"attributes": otlpAttrs(spans[0].Resource().Attributes())},
		"scopeSpans": scopeSpans,
	}}}
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// otlpStatus maps the Go API's codes (Unset, Error, Ok) to OTLP's (UNSET, OK, ERROR).
func otlpStatus(c codes.Code) int {
	switch c {
	case codes.Ok:
		return 1
	case codes.Error:
		return 2
	}
	return 0
}

func otlpAttrs(attrs []attribute.KeyValue) []kv {
	out := []kv{}
	for _, a := range attrs {
		out = append(out, kv{"key": string(a.Key), "value": otlpValue(a.Value)})
	}
	return out
}

func otlpValue(v attribute.Value) kv {
	var vals []kv
	switch v.Type() {
	case attribute.BOOL:
		return kv{"boolValue": v.AsBool()}
	case attribute.INT64:
		return kv{"intValue": strconv.FormatInt(v.AsInt64(), 10)}
	case attribute.FLOAT64:
		return kv{"doubleValue": v.AsFloat64()}
	case attribute.STRING:
		return kv{"stringValue": v.AsString()}
	case attribute.BOOLSLICE:
		for _, x := range v.AsBoolSlice() {
			vals = append(vals, otlpValue(attribute.BoolValue(x)))
		}
	case attribute.INT64SLICE:
		for _, x := range v.AsInt64Slice() {
			vals = append(vals, otlpValue(attribute.Int64Value(x)))
		}
	case attribute.FLOAT64SLICE:
		for _, x := range v.AsFloat64Slice() {
			vals = append(vals, otlpValue(attribute.Float64Value(x)))
		}
	case attribute.STRINGSLICE:
		for _, x := range v.AsStringSlice() {
			vals = append(vals, otlpValue(attribute.StringValue(x)))
		}
	default:
		return kv{"stringValue": v.Emit()}
	}
	return kv{"arrayValue": kv{"values": vals}}
}
