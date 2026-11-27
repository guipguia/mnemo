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

## One hub at a time

Run a single hub, as with SQLite; the chart does (`replicas: 1`, `Recreate`). Webhook delivery
and retention run inside the hub, and two hubs would both do them. Running several hubs
against one database (HA) is on the [roadmap](../ROADMAP.md).

The hub also uses one database connection, so writes are serialized exactly as with SQLite.

## Moving from SQLite

There's no migration tool yet. A new hub on PostgreSQL starts with an empty history. Restart
the agents after switching: on startup they find no resume point and record every object's
current state as a baseline. The versions recorded before stay in the SQLite file.
