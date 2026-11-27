# Metrics

The hub serves Prometheus metrics with `--metrics-listen` (e.g. `:9090`). In the chart, set
`hub.metrics.enabled` (hub and `all` modes). That adds a ClusterIP Service `<release>-metrics`.
`hub.metrics.serviceMonitor.enabled` adds a Prometheus Operator ServiceMonitor, and
`.labels` labels it the way your Prometheus selects them (e.g. `release: kube-prometheus-stack`).

It's a separate listener and Service because metrics name clusters and webhooks and skip the
[access policy](auth.md). Don't expose it on an ingress or a LoadBalancer.

| Metric | Type | Meaning |
|---|---|---|
| `mnemo_build_info{version}` | gauge | Always 1. |
| `mnemo_changes_recorded_total` | counter | Changes recorded since the database was created. |
| `mnemo_ingest_failures_total` | counter | Agent batches the hub rejected or failed to store, since the hub started. |
| `mnemo_db_size_bytes` | gauge | SQLite database size, without the WAL. |
| `mnemo_cluster_last_seen_timestamp_seconds{cluster}` | gauge | When the cluster's agent last checked in: changes, or its one-minute heartbeat. |
| `mnemo_webhook_pending_changes{webhook}` | gauge | Changes recorded after the webhook's position, matching or not. |
| `mnemo_webhook_position{webhook}` | gauge | ID of the last change the webhook handled. It moves only when deliveries succeed. |

Values come from the database at scrape time, so they survive hub restarts, except
`mnemo_ingest_failures_total`. Restoring the database from a lagging backup moves
`mnemo_changes_recorded_total` back, which Prometheus reads as a counter reset.

The heartbeat shows the agent process is alive and can reach the hub, not that its watches are
making progress: an agent stuck on a batch the hub refuses shows up in `mnemo_ingest_failures_total`.

## Alerts

```yaml
- alert: MnemoHubDown
  expr: absent(mnemo_build_info)   # or up{job="..."} == 0
  for: 5m
- alert: MnemoAgentSilent
  expr: time() - mnemo_cluster_last_seen_timestamp_seconds > 300
  for: 5m
- alert: MnemoWebhookStuck       # changes waiting, and none delivered for 15 minutes
  expr: changes(mnemo_webhook_position[15m]) == 0 and mnemo_webhook_pending_changes > 0
  for: 5m
- alert: MnemoIngestFailing
  expr: increase(mnemo_ingest_failures_total[10m]) > 0
  for: 10m
```

`MnemoWebhookStuck` also fires for a webhook whose `delay` exceeds 15 minutes: lengthen the
window to match.
