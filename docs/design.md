# Design notes

mnemo is a recorder, not an operator: it watches, normalizes, diffs, and stores.
It never mutates cluster state.

## Primary query: the change feed

Responders rarely know which object broke. The first question is *"what changed in the last
30 minutes here?"*, so the **cluster-wide change feed** is the home screen and the primary API:

```
GET /changes?cluster=&namespace=&kind=&author=&source=&since=&until=
```

The per-object timeline (`/objects/{cluster}/{gvk}/{ns}/{name}/history`) is the drill-down.
Storage must index changes by `(cluster_id, time)` and `(cluster_id, namespace, time)` first,
by object second.

## Day-one value (cold start)

History is empty at install, so mnemo must show something useful within minutes:

- **Initial sync** stores a baseline snapshot of every watched object, annotated with its most
  recent `managedFields` entry: "last modified by `kubectl` at 09:12" works immediately.
- **Events archive** starts at once, capturing the ~1h of Events Kubernetes still holds.
- Later: backfill from existing audit logs.

Baseline rows are marked so the UI never presents them as changes mnemo observed.

## Topology: agent + hub

Multi-cluster from day one. One binary, three modes:

```
 cluster A: [mnemo agent] ──┐
 cluster B: [mnemo agent] ──┼─HTTPS/mTLS─▶ [mnemo hub] ─▶ storage
 cluster C: [mnemo agent] ──┘                   │
                                         UI / API / kubectl plugin
```

| Mode | Runs | Role |
|---|---|---|
| `mnemo agent` | In every cluster | Watch, normalize, diff, push batches to the hub. Stateless. |
| `mnemo hub` | Usually a management cluster, 1 replica | Owns storage, serves API + UI. |
| `mnemo all` | One pod | Agent + hub in-process. Single-cluster installs and demos. |

**Why push, not pull:** a pulling hub would need admin credentials for every cluster and
network reachability to every API server (private clusters break that). Agents only need
egress to the hub.

**Cluster identity:** `kube-system` namespace UID (stable across renames) + a display name.
Every stored row carries `cluster_id`.

**Agent ↔ hub:** HTTPS with mTLS (or a bearer token for v0.1). On start the agent asks the hub
for its per-GVK resume `resourceVersion`, so the agent keeps no local state.

## Watching

- Raw dynamic watches per GVK with bookmarks, no informers: informers cache every object in
  memory, and the hub's last stored state is already the "before" side of each diff.
- The agent sends normalized full objects plus their `managedFields`; the **hub** dedupes,
  diffs, attributes, and stores. This keeps the agent stateless and makes at-least-once
  delivery safe (a resent object is a no-op).
- Resume from the checkpoint held by the hub.
- Short outages need nothing special: the watch resumes from the checkpoint and replays what
  was missed, with normal attribution.
- `410 Gone` (hub or agent down past the etcd compaction window): relist, compare against the
  hub's latest state, record differences as *"changed while offline"*; objects missing from
  the list become tombstones with author unknown.
- Deletions are stored as tombstones.

## Noise filtering

Drop `status`, `metadata.resourceVersion`, `metadata.managedFields`, `metadata.generation`,
and the `kubectl.kubernetes.io/last-applied-configuration` and `deployment.kubernetes.io/revision`
annotations by default.
Configurable per GVK. Hash the normalized object and skip the write if unchanged.

## Timestamps

A watch event carries no time, so the agent stamps events when it processes them. That is
seconds late normally, but minutes late for events held back while the hub was down. The hub
corrects it from what Kubernetes recorded, moving times earlier, never later:

- `added`: `metadata.creationTimestamp`.
- `modified`: the `managedFields` time of the manager that owns the changed fields. `/scale`
  entries have no time, so those keep the observed time.
- `baseline` and offline catch-up (listed objects): newest non-status `managedFields` time.
- `deleted`: observed time (deletes don't update `managedFields`).

These have 1s precision, so a version is never stored with a time before the previous one.
Audit `stageTimestamp` will replace all of this once audit attribution exists.

## The "who" problem

A watch event says *what* changed, not *who*.

| Source | Gives user? | Notes |
|---|---|---|
| `managedFields` | Field manager only (`kubectl`, `argocd`) | Always available. v0.1. |
| Audit webhook → local agent | Yes, full `userInfo` | Self-managed clusters. v0.2. |
| Cloud audit log readers (EKS/GKE/AKS) | Yes | Managed clusters disallow audit webhooks. Later. |
| Validating admission webhook | Yes | Rejected: in the write path, can block writes when down. |

**How `managedFields` attribution works:** a writer takes ownership of the fields it changes,
so the author of a modification is the manager that owns the changed paths. List items are
matched by any key (`k:{"name":"nginx"}`), since the diff uses indexes. Picking the newest
entry instead was wrong in practice: `/scale` entries carry no timestamp, and timestamps have
1s precision. Fallback when nobody owns the changed paths (removed fields): newest entry.

Joining audit events to changes: exact match on `responseObject.metadata.resourceVersion`
requires `RequestResponse` audit level. Recommend it only for watched GVKs (never Secrets),
with fallback to fuzzy match on objectRef + verb + time window.

## Change source: GitOps vs manual

Every change gets a `source` classification, derived from the field manager (and audit
user agent when available):

| `source` | Field managers (defaults, configurable) |
|---|---|
| `gitops` | `argocd-controller`, `argocd-application-controller`, `kustomize-controller`, `helm-controller` |
| `controller` | Other in-cluster controllers / service accounts |
| `manual` | `kubectl-*`, `kubectl-edit`, `kubectl-client-side-apply`, `Mozilla` (dashboards), humans via audit |
| `unknown` | Offline gap, no attribution |

A `manual` change to an object that is also GitOps-managed is flagged as **drift**:
"someone ran `kubectl edit` in prod at 14:31". For `gitops` changes, link to the commit
using Argo CD (`argocd.argoproj.io/*`, Application status) or Flux (`kustomize.toolkit.fluxcd.io/*`,
revision) metadata where present.

## Storage

**Model (v0.1):** one row per change holding the full normalized object, plus the list of
changed JSON pointers (e.g. `/spec/replicas`) for the feed. State at T = one indexed lookup.
Unchanged objects are never stored, so size grows with real changes only.

**Later, if storage size measurably matters:** full snapshot every N changes + RFC 6902
JSON patches between; state at T = nearest snapshot ≤ T + replay patches.

**Identity:** history is keyed by cluster + group + kind + namespace + name, not UID, so a
delete and re-create is one timeline. Change types: `baseline` (first seen in a list),
`added`, `modified`, `deleted` (tombstone).

**Backends:** SQLite first, PostgreSQL and MySQL later.

- Hub code goes through the `store` package; the API/UI never see SQL. The Go interface is
  extracted when the second backend lands.
- Plain `database/sql`, no ORM. Keep SQL portable: store objects/patches as text/blob,
  no backend-specific JSON operators in the hot path.
- Migrations kept per dialect (`migrations/{sqlite,postgres,mysql}/`).
- One shared conformance test suite run against every backend in CI.

**SQLite operational rules:**
- File on a PVC (e.g. `/data/mnemo.db`), WAL mode.
- Block storage only (EBS, PD, Azure Disk). Never NFS/EFS/Azure Files: unreliable locking corrupts SQLite.
- Single writer → hub runs 1 replica. Postgres/MySQL lift this and enable HA.
- Helm chart sets `helm.sh/resource-policy: keep` on the PVC so uninstall doesn't delete history.
- Backups: optional Litestream sidecar replicating to S3/GCS/Azure Blob, restore on start.

| Event | SQLite data survives? |
|---|---|
| Pod restart / upgrade | Yes |
| Rescheduled to another node | Usually; zonal volumes only follow within the zone |
| `helm uninstall` | Yes (PVC kept) |
| Namespace/cluster deleted | No, restore from Litestream |
| `emptyDir` (demo mode) | No |

## Secrets

Excluded by default. If enabled: store key names + value hashes only, never values.
Enough to say "`DB_PASSWORD` rotated at 14:31".

## Events and rollouts

- Kubernetes Events expire after ~1h; mnemo keeps them in an append-only table with its own retention.
- Rollouts: Deployment → ReplicaSet → Pod via `ownerReferences` and the
  `deployment.kubernetes.io/revision` annotation, shown on the same timeline as config changes.

## Outbound webhooks

The hub can POST to configured URLs when it records a change matching a filter
(cluster, GVK, namespace, name, change type), e.g. "notify Slack when any ClusterRoleBinding
changes in prod". Payload: object ref, author, timestamp, patch, link to the UI.

- At-least-once delivery with retries and backoff; receivers dedupe on a change ID.
- Optional HMAC signature header so receivers can verify the sender.
- Fired after the change is committed, so a slow receiver never blocks recording.

Not to be confused with the inbound audit webhook used for attribution.

## Read access

- **Who:** `--auth none` (port-forward, admin-only), `oidc` (built-in login), or `proxy`
  (headers from an auth proxy, trusted only from configured CIDRs). See [auth.md](auth.md).
- **What:** a policy maps users and IdP groups to clusters × namespaces. Enforced in SQL for
  the feed (so limits stay correct) and per object elsewhere. Denied objects look missing.
- **Why not Kubernetes RBAC (yet):** the hub has no credentials for agent clusters, so it
  can't run a SubjectAccessReview, and IdP identities often don't match cluster identities
  (e.g. EKS IAM). Evaluating the RBAC objects mnemo already records is a possible later mode.
- `kubectl mnemo` plugin, e.g. `kubectl mnemo diff deploy/api --at "yesterday 14:32"`.

## Integrations

Meet responders where they already are; the mnemo UI is not the only surface.

- **Grafana:** annotations from the change feed (data source or annotation API), so changes
  appear as markers on existing dashboards.
- **Headlamp:** plugin showing the object timeline inside Headlamp (a CNCF project).
- **MCP server / query API for AI agents:** "what changed before this alert?" as a tool call.
  Read-only, same RBAC rules as the UI.
- **Slack and others:** via outbound webhooks.

## Security posture

The agent reads every watched object in every cluster, so trust is a feature:

- Read-only ClusterRole (`get`, `list`, `watch` on configured GVKs only); exact rules
  documented and generated from config.
- Hub needs no Kubernetes credentials for remote clusters (agents push).
- Images signed with cosign, SBOM per release, SLSA provenance.
- Published resource usage and scale numbers (RAM/CPU/storage per 10k objects, changes/sec).

## Positioning

Closest alternatives: Komodor (commercial SaaS change timeline), Robusta (change tracking
and alerting), raw audit logs in a log store.

mnemo: **open source, self-hosted, data never leaves your infrastructure, and point-in-time
reconstruction of any object**, not only notification that something changed.

## Non-goals

- Replacing audit logging or SIEM.
- Restoring/rolling back objects (read-only first).
- Storing Secret values.
