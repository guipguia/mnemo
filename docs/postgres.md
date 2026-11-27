# PostgreSQL

The hub keeps history in SQLite by default. To use PostgreSQL instead (CI tests 17), give it a
URL with `--db` or `$MNEMO_DB`:

```sh
MNEMO_DB='postgres://mnemo:...@db.example.com:5432/mnemo?sslmode=verify-full' mnemo hub --token ...
```

Only URLs (`postgres://` or `postgresql://`) are recognized; anything else is a SQLite file
path. The URL takes the usual [libpq parameters](https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-PARAMKEYWORDS),
such as `sslmode` and `search_path`. The hub creates its tables on startup, so its user needs
`CREATE` on the schema. Logs show the URL with the password hidden, in the user info or as `?password=`.

Chart: put the URL in a Secret under the key `url` and set `hub.database.existingSecret`. No
PVC is created, and `hub.backup` (Litestream, for SQLite) is refused: back up PostgreSQL the
way you back up your other databases.

## Several hubs

On PostgreSQL you can run several hubs on one database, for availability (chart:
`hub.replicas`, in mode `hub`). Put them behind one Service; any hub takes agent pushes and
serves the UI, API and MCP.

- **Writes** take a database-wide lock, so hubs write one at a time, as a single hub does. This
  is what keeps an object's versions in order when a retried push lands on two hubs, and
  keeps change IDs committing in order, so webhook delivery never skips one. Add hubs for
  availability, not for write throughput.
- **Webhooks and retention** run on one hub, the leader, which holds a PostgreSQL advisory
  lock on a connection of its own. If it dies or loses the database, another hub takes over
  within about 5 seconds and resumes webhook delivery from the stored position. While a
  partitioned old leader notices, both may deliver for a few seconds: delivery is
  at-least-once, and receivers dedupe on `id`.
- **Logins** (`--auth oidc`) are signed cookies: give every hub the same
  `MNEMO_SESSION_KEY` (the chart does), or a login on one hub fails on another.
- `mnemo_ingest_failures_total` is per hub; the other metrics read the shared database.

Each hub uses one connection for its work and one for the leader lock.

## Moving from SQLite

There's no migration tool yet. A new hub on PostgreSQL starts with an empty history. Restart
the agents after switching: on startup they find no resume point and record every object's
current state as a baseline. The versions recorded before stay in the SQLite file.
