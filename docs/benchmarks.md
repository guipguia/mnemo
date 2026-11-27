# Benchmarks

How fast the hub stores changes and answers queries, and how much space history takes.
Measured 2026-11-27 on a laptop: Intel Core Ultra 9 285H (16 threads), 32 GB RAM, NVMe SSD.
SQLite in WAL mode on that disk. PostgreSQL 17 in a local container with default settings.
Reproduce with the commands below; numbers on your hardware will differ.

## Ingest

| Path | SQLite | PostgreSQL |
|---|---|---|
| Store one modified version (diff, attribution, own transaction) | ~7,400 versions/s | ~400 versions/s |
| End to end: agent batch of 100 over HTTP, JSON, managedFields attribution | ~5,000 objects/s | |

Each version is its own transaction, so PostgreSQL's rate is set by its commit latency
(here ~2.5 ms per commit, in a container); a server with faster commits goes proportionally
faster. For scale: a busy cluster produces tens of changes per second, and an initial sync of
10,000 objects takes a few seconds on SQLite and under half a minute on PostgreSQL.

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

Not measured yet: agent memory and CPU per watched object on a real cluster.
