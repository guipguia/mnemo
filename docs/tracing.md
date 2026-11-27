# OpenTelemetry traces

mnemo exports traces over OTLP/HTTP when the standard OpenTelemetry endpoint variable is
set, in any mode. Without it (or with `OTEL_SDK_DISABLED=true` or
`OTEL_TRACES_EXPORTER=none`), tracing is off: servers aren't wrapped, and agent pushes pay
only a no-op span.

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 mnemo hub ...
```

With the Helm chart:

```yaml
extraEnv:
- {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "http://otel-collector.monitoring:4318"}
```

## What is traced

| Span | Where | Attributes |
|---|---|---|
| `GET /api/v1/changes`, `POST /api/v1/ingest`, ... | hub, one server span per request, named by route | HTTP semantic conventions |
| `ingest` | hub, the batch's database transaction | `mnemo.cluster`, `mnemo.stream`, `mnemo.items`, `mnemo.events` |
| `HTTP POST` | agent pushes to the hub | HTTP semantic conventions |

Agent pushes carry a W3C `traceparent`, so an agent's push and the hub's handling of it are
one trace. Other requests (UI, API, MCP) start a new trace, linked to the caller's
`traceparent` if they sent one: they come from anyone, who shouldn't choose trace IDs or
force sampling. Webhook deliveries aren't traced: their URLs often hold secrets (a Slack
webhook's path is its credential), and receivers are third parties.

## Configuration

These standard variables are read:

| Variable | Default | |
|---|---|---|
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | | Full URL, used as is. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | | Base URL; `/v1/traces` is appended. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | | `key=value,...`, values percent-encoded, e.g. `Authorization=Bearer%20xyz`. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | `parentbased_always_on` | e.g. `parentbased_traceidratio` and `0.1`. |
| `OTEL_SERVICE_NAME` | `mnemo` | |
| `OTEL_RESOURCE_ATTRIBUTES` | | Added to `service.version` and `mnemo.mode` (hub, agent, all). Malformed entries are skipped with a warning. |
| `OTEL_SDK_DISABLED`, `OTEL_TRACES_EXPORTER` | | `true` / `none` turn tracing off. |

## Limits

- **OTLP/HTTP with JSON only**, which every OTLP receiver accepts (the OpenTelemetry
  Collector, Jaeger, Tempo, most vendors). No gRPC: the official exporter would add 11 MB
  to the binary for it. Point gRPC-only backends at a collector.
- No retries: if the receiver refuses or can't be reached, that batch of spans is dropped
  (and logged).
- Not supported: `OTEL_EXPORTER_OTLP_PROTOCOL` other than `http/json` (warned about),
  `..._CERTIFICATE`, `..._CLIENT_KEY`, `..._CLIENT_CERTIFICATE`, `..._TIMEOUT` (10s) and
  `..._COMPRESSION`. For TLS to the collector, `https://` endpoints use the system's roots.
- Traces only. Metrics are Prometheus ([metrics.md](metrics.md)).
