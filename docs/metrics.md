# Metrics

The hub serves Prometheus metrics with `--metrics-listen` (e.g. `:9090`; chart:
`hub.metrics.enabled`, plus `hub.metrics.serviceMonitor` for the Prometheus Operator).

It's a separate listener because metrics name clusters and webhooks and skip the
[access policy](auth.md). Don't expose it on an ingress.

| Metric | Type | Meaning |
|---|---|---|
| `mnemo_build_info{version}` | gauge | Always 1. |
| `mnemo_changes_recorded_total` | counter | Changes recorded since the database was created. |
| `mnemo_ingest_failures_total` | counter | Agent batches the hub rejected or failed to store, since the hub started. |
| `mnemo_db_size_bytes` | gauge | SQLite database size, without the WAL. |
| `mnemo_cluster_last_seen_timestamp_seconds{cluster}` | gauge | When the cluster's agent last pushed or resumed. |
| `mnemo_webhook_pending_changes{webhook}` | gauge | Changes recorded after the webhook's position, matching or not. |

Values come from the database at scrape time, so they survive hub restarts, except
`mnemo_ingest_failures_total`.

## Alerts

```yaml
- alert: MnemoAgentSilent       # agents send a heartbeat every minute, even when nothing changes
  expr: time() - mnemo_cluster_last_seen_timestamp_seconds > 900
- alert: MnemoWebhookStuck      # a receiver keeps failing; later changes wait behind it
  expr: min_over_time(mnemo_webhook_pending_changes[15m]) > 0 and deriv(mnemo_webhook_pending_changes[15m]) >= 0
- alert: MnemoIngestFailing
  expr: increase(mnemo_ingest_failures_total[10m]) > 0
```
