# Outbound webhooks

The hub can POST to your URLs when it records a change that matches a filter: "tell Slack
when any ClusterRoleBinding changes in prod", "page on drift in `payments`".

Not to be confused with the inbound [audit webhook](audit.md) used for attribution.

## Configure

Pass a webhooks file with `--webhooks` (chart: put it in a Secret under the key
`webhooks.yaml` and set `hub.webhooks.secret`). It holds receiver URLs and HMAC secrets, so
keep it in a Secret. The hub reads it at startup.

```yaml
url: https://mnemo.example.com      # optional: payloads link to the change in the UI
webhooks:
  - name: prod-rbac                 # unique; keys the delivery position (see below)
    url: https://hooks.slack.com/services/T000/B000/XXXX
    secret: s3cret                  # optional: sign payloads with HMAC-SHA256
    delay: 35s                      # optional: hold changes back, see "Attribution"
    match:
      clusters: [prod]
      kinds: [ClusterRoleBinding, ClusterRole]
  - name: payments-drift
    url: https://alerts.example.com/mnemo
    match:
      namespaces: ["payments-*"]
      driftOnly: true
```

`match` fields (all optional; empty matches everything, and the fields combine with AND):

| Field | Matches |
|---|---|
| `clusters`, `namespaces` | Patterns as in the [access policy](auth.md): a name, `*`, or `prefix*`. With `namespaces` set, cluster-scoped objects never match. |
| `kinds`, `names` | Exact `kind` (e.g. `ConfigMap`) and object names. |
| `types` | `added`, `modified`, `deleted`. |
| `sources` | `gitops`, `controller`, `manual`, `unknown`. |
| `driftOnly` | Only manual changes to GitOps-managed objects. |

Baselines (objects found when mnemo starts watching) are not changes and never match.

## Delivery

- After the change is stored, never on the ingest path: a slow receiver can't hold up
  recording.
- **At least once, in order.** Each webhook keeps its position in the change log in the
  database. A change is retried until the receiver answers 2xx (backoff from 1s up to 5
  minutes), and later changes wait behind it. After a hub restart, delivery picks up where
  it stopped. Receivers dedupe on `id`.
- A new webhook starts from the moment it is added, not from the start of history.
  Renaming a webhook makes it a new one.
- A receiver that is down for longer than your retention policy misses the changes that
  expired meanwhile.

## Payload

```http
POST /mnemo HTTP/1.1
Content-Type: application/json
X-Mnemo-Change: 4213
X-Mnemo-Signature: sha256=8f2a...
```

```json
{
  "id": 4213,
  "cluster": "prod", "group": "rbac.authorization.k8s.io", "kind": "ClusterRoleBinding",
  "namespace": "", "name": "admin",
  "version": "v1", "uid": "…", "resourceVersion": "981234",
  "time": "2026-10-20T14:31:07Z",
  "type": "modified",
  "author": "alice@example.com", "source": "manual", "drift": false,
  "paths": ["/subjects/0/name"],
  "link": "https://mnemo.example.com/?change=4213&cluster=prod&…",
  "text": "prod: ClusterRoleBinding admin modified by alice@example.com (manual) https://mnemo.example.com/?…"
}
```

`text` is a ready-made line for chat tools that read a `text` field, such as Slack incoming
webhooks. For other formats, put a small relay in front. The full object is not sent: follow
`link`, or call `GET /api/v1/changes/{id}`.

### Verifying the signature

`X-Mnemo-Signature` is `sha256=` followed by the hex HMAC-SHA256 of the raw body, keyed with
`secret`. Compare it in constant time:

```python
expected = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
ok = hmac.compare_digest(request.headers["X-Mnemo-Signature"], expected)
```

## Attribution

With [audit attribution](audit.md), the person behind a change arrives a little after the
change itself, because the API server batches audit events (up to 30s by default,
`--audit-webhook-batch-max-wait`). A webhook that fires right away names the tool
(`kubectl-edit`). Set `delay` a bit above the batch wait so payloads name the person.
