# Benchmarks

How fast the hub stores changes and answers queries, and how much space history takes.
Measured 2026-11-27 on a laptop: Intel Core Ultra 9 285H (16 threads), 32 GB RAM, NVMe SSD.
SQLite in WAL mode on that disk. PostgreSQL 17 in a local container with default settings.
Reproduce with the commands below; numbers on your hardware will differ.

## Ingest

| Path | SQLite | PostgreSQL |
|---|---|---|
| Store one modified version (diff, attribution, own transaction) | ~8,500 versions/s | ~420 versions/s |
| End to end: agent batch of 100 over HTTP, JSON, managedFields attribution | ~4,000 objects/s | |

Each version is its own transaction, so PostgreSQL's rate is set by its commit latency
(here ~2.3 ms per commit, in a container); a server with faster commits goes proportionally
faster. For scale: a busy cluster produces tens of changes per second, and an initial sync of
10,000 objects takes a few seconds on SQLite and under half a minute on PostgreSQL.

The hub used under 4 MB of Go heap during the end-to-end run.

## Queries at 1 million versions

10 clusters × 20 namespaces × 50 Deployments, 100 versions each, over 30 days; 5% manual
changes (drift). Median time per query:

| Query | SQLite | PostgreSQL |
|---|---|---|
| Change feed, newest 100 (the home screen) | 0.46 ms | 0.58 ms |
| Feed, one cluster, last hour | 0.50 ms | 0.50 ms |
| Feed, drift only | 1.4 ms | 0.72 ms |
| Feed under a team policy (2 clusters, `ns-1*`) | 0.65 ms | 0.66 ms |
| One object's history (100 versions) | 0.51 ms | 0.57 ms |
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

The query benchmarks load `MNEMO_BENCH_N` versions first (default 100,000), which takes
minutes at 1 million. On PostgreSQL they use the database's `public` schema and empty its
tables first: point them at a scratch database.

Not measured yet: agent memory and CPU per watched object on a real cluster.
