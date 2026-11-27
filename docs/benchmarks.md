# Benchmarks

How fast the hub stores changes and answers queries, and how much space history takes.
Measured 2026-11-27 on a laptop: Intel Core Ultra 9 285H (16 threads), 32 GB RAM, NVMe SSD.
SQLite in WAL mode on that disk. PostgreSQL 17 in a local container with default settings.
Reproduce with the commands below; numbers on your hardware will differ.

## Ingest

| Path | SQLite | PostgreSQL |
|---|---|---|
| Store one modified version (diff, attribution, own transaction) | ~7,000 versions/s | ~500 versions/s |
| End to end: agent batch of 100 over HTTP, JSON, managedFields attribution | ~7,800 objects/s | ~2,800 objects/s |

Storing one version alone is its own transaction, so PostgreSQL's rate there is set by its
commit latency (here ~2.5 ms per commit, in a container); a server with faster commits goes
proportionally faster. End to end, a batch is one transaction, and PostgreSQL's rate is set
by the round trips each version makes (reading the previous version, writing the new one).
Both rates hold as an object's history grows: the previous version is read from the index,
not found by sorting the object's versions (which made ingest slow down as history grew,
to ~1,800 and ~900 objects/s at 800 versions per object, before that was fixed). For scale:
a busy cluster produces tens of changes per second, and an initial sync of 10,000 objects
takes a few seconds on either backend.

The whole test process (hub and client) had under 4 MB of Go heap in use after the end-to-end run.

## Queries at 1 million versions

10 clusters × 20 namespaces × 50 Deployments, 100 versions each, over 30 days; 5% manual
changes (drift). Mean time per query:

| Query | SQLite | PostgreSQL |
|---|---|---|
| Change feed, newest 100 (the home screen) | 0.37 ms | 0.53 ms |
| Feed, one cluster, last hour | 0.38 ms | 0.75 ms |
| Feed, drift only | 1.6 ms | 0.74 ms |
| Feed under a team policy (2 clusters, `ns-1*`) | 0.63 ms | 0.63 ms |
| One object's history (100 versions) | 0.47 ms | 0.67 ms |
| One object at a point in time | 0.03 ms | 0.06 ms |

## Storage

About **2 KB per version** for a ~2.5 KB Deployment (SQLite 2.2 KB, PostgreSQL 1.9 KB, with
indexes): every version stores the full normalized object, so any point in time is one
lookup. 1 million versions is about 2 GB. Cap it with retention (`hub.retention.*`).

## Reproduce

```sh
# Store benchmarks: SQLite, or PostgreSQL with MNEMO_TEST_POSTGRES=postgres://...
MNEMO_BENCH_N=1000000 go test ./internal/store -run '^$' -bench . -benchtime 2000x -v -timeout 60m
# End-to-end ingest over HTTP
go test ./internal/hub -run '^$' -bench IngestHTTP -benchtime 200x
```

The query benchmarks load `MNEMO_BENCH_N` versions first (default 100,000), which takes a few
minutes at 1 million. On PostgreSQL each benchmark works in a schema of its own, dropped
afterwards, and loads without waiting for commits to reach disk (only the setup: timed work
uses normal commits).

The `changes_ts` index behind the feed numbers is created at startup like the rest of the
schema; on an existing large database the first start after upgrading builds it (about 0.3 s
per million versions on PostgreSQL here) before serving.

## Agent on a real cluster

A `mnemo agent` outside the cluster, watching the default resources on a local
[kind](https://kind.sigs.k8s.io) cluster (Kubernetes 1.37, in a container) and pushing to a
local hub on SQLite. The objects are ConfigMaps of ~1.2 KB, 16 keys each.

| | Agent memory (RSS) | Agent CPU |
|---|---|---|
| Idle, ~240 objects | 27 MB | |
| Initial sync of 10,000 more objects: 4.5 s | 40 MB peak | 1.3 s |
| Watching 10,000 objects | 29 MB | |
| 5,000 updates in 23 s (`kubectl apply`), recorded as they happen | 29 MB | 1.7 s (~0.34 ms per change, pushed in batches) |
| Restart with 10,000 objects: resumes from its checkpoints, no relist | 22 MB | ~0 s |

- **Memory barely grows with the number of objects watched:** the agent keeps no copy of
  them (the hub compares versions). A relist reads 500 objects at a time and keeps only each
  object's name until the end, about 50 bytes each: before paging, the same initial sync
  peaked at 247 MB.
- Pages are pushed at the hub's pace. If a relist outlasts the API server's snapshot (etcd
  compacts every 5 minutes by default; at PostgreSQL's ~2,800 objects/s that is several
  hundred thousand objects in one resource, or a hub outage mid-relist), the agent reads
  every page again first, holding them converted, and pushes them after. That holds about
  2 KB per object for these ConfigMaps (more for larger objects): around 400 MB at 200,000,
  and up to about twice that in RSS before Go collects. Raise the agent's memory limit
  (512Mi by default) for resources that large.
- **CPU follows the change rate:** a busy cluster's tens of changes per second cost a few
  percent of a core.
- The hub used 36 MB at most through all of the above.

The chart's default requests (`50m` CPU, `64Mi` memory) fit this; give more memory
headroom on clusters with many large objects (a page of 500 big CRs).

To reproduce: create a kind cluster, run `mnemo hub` and `mnemo agent --kubeconfig` locally,
apply ConfigMaps in bulk (`kubectl apply --server-side -f list.json`), and read `VmHWM`,
`VmRSS` and the CPU times from `/proc/<agent pid>/status` and `/stat`.
