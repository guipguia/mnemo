# kubectl plugin

`kubectl mnemo` reads the hub's history from the terminal. It only reads, and it sees what the
hub's [access policy](auth.md) lets you see.

## Install

Release archives include `kubectl-mnemo`. Put it on your `PATH`, or build it with
`go build -o ~/.local/bin/kubectl-mnemo ./cmd/kubectl-mnemo`, then point it at the hub:

```sh
export MNEMO_HUB=https://mnemo.example.com
```

## Use

```sh
kubectl mnemo changes --since 1h --drift            # the change feed, across clusters
kubectl mnemo changes --cluster prod -n shop --kind ConfigMap --source manual
kubectl mnemo history deploy/api                    # every recorded version
kubectl mnemo get cm/app --at 2026-10-20T14:32:00Z  # the YAML as it was
kubectl mnemo diff deploy/api --at 2h               # what changed in the last two hours
kubectl mnemo diff deploy/api --at 3h --to 2h       # between two points
```

Times are RFC 3339, or a duration ago (`2h`, `90m`). `diff` runs the system `diff` and, like it,
exits 1 when the two sides differ and 2 on errors, such as an unreachable hub.

## Which object

`deploy/api` resolves as in `kubectl`: discovery on the current context expands the short name,
and the namespace comes from `-n` or the context.

The cluster is `--cluster`, then `$MNEMO_CLUSTER`, then the context name. It has to match the
name the agent runs with (`--cluster`), which often differs from the context, so set
`MNEMO_CLUSTER` when it does. `changes` needs none of this, ignores `$MNEMO_CLUSTER`, and
defaults to every cluster and namespace unless you pass `--cluster` or `-n`.

`deploy.apps/api` names the group as well, as in `kubectl`.

History matters most when the cluster is down or the kind is gone. `--group` skips the cluster
entirely, and takes the kind literally:

```sh
kubectl mnemo history Deployment/api --group apps --cluster prod -n shop
kubectl mnemo get ConfigMap/app --group "" --cluster prod -n shop --at 2h
```

## Logging in

With `--auth none` there's nothing to do. Otherwise pass what the hub expects with `--header`
(repeatable), or one header in `$MNEMO_HEADER`, which keeps it out of your shell history:

- **`--auth oidc`:** copy the `mnemo_session` cookie from your browser after logging in:
  `MNEMO_HEADER='Cookie: mnemo_session=...'`. It lasts as long as `--session-ttl`, and it's
  your login, so treat it like a password.
- **`--auth proxy`:** whatever your auth proxy accepts, such as
  `Authorization: Bearer ...` for oauth2-proxy with `--skip-jwt-bearer-tokens`.
