# OpenTelemetry traces

mnemo exports traces over OTLP/HTTP when the standard OpenTelemetry endpoint variable is
set, in any mode. Without it, tracing is off and costs nothing.

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
| `HTTP POST` | agent pushes to the hub, and hub webhook deliveries | HTTP semantic conventions |

Agent pushes carry a W3C `traceparent`, so an agent's push and the hub's handling of it are
one trace. The hub also continues traces from incoming requests that carry one (an
ingress or a UI behind a traced proxy).

## Configuration

These standard variables are read:

| Variable | Default | |
|---|---|---|
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | | Full URL, used as is. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | | Base URL; `/v1/traces` is appended. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | | `key=value,...`, values URL-encoded, e.g. `Authorization=Bearer%20xyz`. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | `parentbased_always_on` | e.g. `parentbased_traceidratio` and `0.1`. |
| `OTEL_SERVICE_NAME` | `mnemo` | |
| `OTEL_RESOURCE_ATTRIBUTES` | | Added to `service.version` and `mnemo.mode` (hub, agent, all). |

## Limits

- **OTLP/HTTP with JSON only**, which every OTLP receiver accepts (the OpenTelemetry
  Collector, Jaeger, Tempo, most vendors). No gRPC: the official exporter would add 11 MB
  to the binary for it. Point gRPC-only backends at a collector.
- No retries: if the receiver refuses or can't be reached, that batch of spans is dropped
  (and logged).
- Traces only. Metrics are Prometheus ([metrics.md](metrics.md)).
