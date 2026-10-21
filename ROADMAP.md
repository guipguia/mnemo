# Roadmap

Design details: [docs/design.md](docs/design.md).

## v0.1: prove the idea
- [ ] Single binary with `agent`, `hub`, and `all` modes
- [ ] Agent: dynamic watches on configured GVKs, noise filtering, push normalized objects to hub (hub dedupes and diffs)
- [ ] Multi-cluster: cluster identity, per-cluster resume checkpoints held by the hub
- [ ] Storage: SQLite behind a `Store` interface, full-object rows + changed paths, tombstones
- [ ] Initial sync baseline with last-modified info from `managedFields` (day-one value)
- [ ] Attribution via `managedFields`; change `source` classification (gitops / controller / manual)
- [ ] Drift flag: manual changes to GitOps-managed objects
- [ ] API: cluster-wide change feed (primary), object history, object state at time T
- [ ] UI: change feed home screen, per-object timeline, diff view
- [ ] Read-only ClusterRole generated from config, documented
- [ ] Helm chart (PVC kept on uninstall), kind-based multi-cluster demo
- [ ] Scripted "broken ConfigMap" incident demo + README GIF

## v0.2: who and why
- [ ] Audit webhook receiver in the agent for user attribution
- [ ] Events archive and rollout correlation on the timeline
- [ ] Link GitOps changes to Git commits (Argo CD, Flux)
- [ ] Outbound webhooks on matching changes (filters, retries, HMAC signatures)
- [ ] Grafana annotations integration
- [ ] Retention policy (age / count)
- [ ] Litestream backup option in the chart
- [ ] `kubectl mnemo` plugin
- [ ] Release supply chain: cosign signatures, SBOM, SLSA provenance

## v0.3: production-ish
- [ ] PostgreSQL backend (enables hub HA)
- [ ] MySQL backend
- [ ] Shared storage conformance suite run against every backend in CI
- [ ] UI and API respect caller RBAC (SubjectAccessReview per cluster)
- [ ] Agent ↔ hub mTLS
- [ ] Prometheus metrics, OpenTelemetry traces
- [ ] MCP server for AI incident agents
- [ ] Headlamp plugin
- [ ] Published scale and resource benchmarks

## Later
- [ ] Cloud audit log readers (EKS, GKE, AKS) for attribution on managed clusters
- [ ] Backfill history from existing audit logs

## CNCF Sandbox readiness
- [ ] 2+ maintainers from different organizations
- [ ] OpenSSF Best Practices "passing" badge
- [ ] Public adopters in [ADOPTERS.md](ADOPTERS.md)
