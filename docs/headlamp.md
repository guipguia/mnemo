# Headlamp plugin

[Headlamp](https://headlamp.dev) is a Kubernetes UI (a CNCF project). The mnemo plugin adds
a **History (mnemo)** section to every resource's page in Headlamp. For each recorded
version it shows when it changed, the kind of change, who made it, how (GitOps,
controller, by hand, or drift) and which fields changed. An optional link opens mnemo
itself, with diffs and the object at any time.

## How it reaches mnemo

Through the Kubernetes API server's service proxy, with the Headlamp user's own
credentials: `/api/v1/namespaces/<ns>/services/<hub>:http/proxy/api/v1/history?...`.
Headlamp needs no extra network access and no CORS, and nobody logs in a second time.

**The hub must run with `--auth none`** (the chart's default). The proxy reaches the hub
from the API server, so no `--auth oidc` cookie or `--auth proxy` header comes with the
request. With those modes the section says it can't sign in: use the "mnemo UI URL" link
instead.

### What that means for access

- **mnemo's own access policy doesn't apply.** With `--auth none` the hub serves its whole
  history to whoever reaches it. That includes full recorded objects (`/api/v1/at`,
  `/api/v1/changes/{id}`): every ConfigMap in every namespace and cluster, including ones
  the user can't `get` in Kubernetes. Through the proxy, the only gate is Kubernetes RBAC.
  So grant `services/proxy` on the hub as you would grant reading everything mnemo records.
- **Limit who can reach the hub directly.** Any pod, or an Ingress in front of the hub,
  bypasses RBAC entirely. Allow the hub's `http` port only from the API server (for the
  proxy) and from the agents, for example with a NetworkPolicy:

  ```yaml
  apiVersion: networking.k8s.io/v1
  kind: NetworkPolicy
  metadata: {name: mnemo-hub, namespace: mnemo}
  spec:
    podSelector: {matchLabels: {app.kubernetes.io/name: mnemo}}
    policyTypes: [Ingress]
    ingress:
    - from: [{ipBlock: {cidr: 172.18.0.0/16}}]   # your control plane's addresses
      ports: [{port: 8080}]
  ```

  How the API server's traffic appears to a NetworkPolicy depends on the cluster (its node
  IPs, or a managed control plane's range): check yours. The policy covers every port of
  the pod, so also allow what you enable: `metrics` (9090) from Prometheus, the audit
  webhook (8443) from the API server, and `agents` (8444, `--agent-listen` with mTLS, see
  [mtls.md](mtls.md)) from remote agents.
- If you need mnemo's policy per user, use the mnemo UI with `--auth oidc` or `proxy`
  instead of this plugin.

### Permissions

The plugin needs `get` on `services/proxy` for the hub's Service. To find the hub by its
`app.kubernetes.io/name=mnemo` label it also needs `list` on `services` in that namespace,
or across the cluster. Setting the hub's namespace and Service in the plugin settings
removes the need for `list`.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: mnemo-history, namespace: mnemo}
rules:
- apiGroups: [""]
  resources: [services/proxy]
  resourceNames: [mnemo]
  verbs: [get]
```

Bind it with a RoleBinding in the hub's namespace.

The viewed cluster is matched to mnemo's record by its `kube-system` namespace UID, which
is the same identity the agent uses, so the names in Headlamp and in mnemo don't have to
agree. Only if that namespace can't be read does the plugin go by name. If the hub runs in
another cluster that Headlamp also knows, set it in the plugin settings.

## Install

Every release attaches `mnemo-headlamp-<version>.tar.gz`, listed in `checksums.txt`, which
is signed with cosign like the binaries (see [SECURITY.md](../SECURITY.md)).

**Headlamp desktop:** check the tarball against `checksums.txt`, extract it into the
plugins folder and restart Headlamp.

```sh
sha256sum --ignore-missing -c checksums.txt
dir=~/.config/Headlamp/plugins/mnemo-headlamp
# macOS: ~/Library/Application Support/Headlamp/plugins/mnemo-headlamp
# Windows: %APPDATA%\Headlamp\Config\plugins\mnemo-headlamp
mkdir -p "$dir" && tar -xzf mnemo-headlamp-*.tar.gz --strip-components=1 -C "$dir"
```

**Headlamp in a cluster:** the plugin runs in users' browsers with their Kubernetes
credentials, so fetch it with TLS verification and check its checksum. For example, with the
Headlamp chart (`config.pluginsDir` defaults to `/headlamp/plugins`):

```yaml
initContainers:
- name: mnemo-plugin
  image: docker.io/curlimages/curl:8.23.0@sha256:3fb7e13e5d179a845c97d78204f3f1064d1907037f26a3ebf91710571151fd93
  env: [{name: V, value: "0.3.0"}]
  command: [sh, -euc]
  args:
  - |
    cd /tmp
    base=https://github.com/guipguia/mnemo/releases/download/v$V
    curl -fsSLO "$base/mnemo-headlamp-$V.tar.gz" -fsSLO "$base/checksums.txt"
    grep " mnemo-headlamp-$V.tar.gz\$" checksums.txt | sha256sum -c -
    mkdir -p /plugins/mnemo-headlamp
    tar -xzf "mnemo-headlamp-$V.tar.gz" --strip-components=1 -C /plugins/mnemo-headlamp
  volumeMounts: [{name: plugins, mountPath: /plugins}]
volumeMounts: [{name: plugins, mountPath: /headlamp/plugins}]
volumes: [{name: plugins, emptyDir: {}}]
```

## Settings

Headlamp → Settings → Plugins → mnemo-headlamp:

| Setting | Default | |
|---|---|---|
| Hub cluster | the cluster being viewed | The Headlamp cluster the hub runs in. |
| Hub namespace, Hub Service | found by label | Set both to skip the lookup. |
| mnemo UI URL | none | e.g. `https://mnemo.example.com`, for "Open in mnemo" links. |

## Develop

`package.json` overrides `tinypool` and `webpack-dev-middleware` to patched majors that the
plugin toolkit doesn't ask for yet: drop them once `@kinvolk/headlamp-plugin` moves to
vitest 4.

```sh
cd headlamp
npm ci
npm start   # rebuilds into Headlamp's plugins folder on change
npm run lint && npm run tsc && npm test && npm run build
```
