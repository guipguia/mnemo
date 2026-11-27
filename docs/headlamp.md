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

- **Who may see history is Kubernetes RBAC:** `get` on `services/proxy` for the hub's
  Service. Grant it like any other permission; without it, the section says so.
- **The hub must not ask for its own login on that path**, so this works with the hub's
  `--auth none`, which is the chart's default. The proxy reaches the hub from the API
  server, so no `--auth oidc` cookie or `--auth proxy` header comes with the request. With
  those modes, the section explains that it can't sign in. Use the "mnemo UI URL" link
  instead.
- **mnemo's own access policy doesn't apply on this path:** anyone with `services/proxy`
  on the hub sees all of its history. Grant that permission as you would grant access to
  the hub itself.

The hub is found by its Service's `app.kubernetes.io/name=mnemo` label, in the cluster you
are viewing. If it runs in another cluster that Headlamp also knows, set it in the plugin
settings. The viewed cluster is matched to mnemo's record by its `kube-system` namespace
UID, which is the same identity the agent uses, so the names in Headlamp and in mnemo
don't have to agree.

## Install

Every release attaches `mnemo-headlamp-<version>.tar.gz`, which is checksummed and signed
with the binaries.

**Headlamp desktop:** extract it into the plugins folder and restart Headlamp.

```sh
dir=~/.config/Headlamp/plugins/mnemo-headlamp   # macOS: ~/Library/Application Support/Headlamp/plugins
mkdir -p "$dir" && tar -xzf mnemo-headlamp-*.tar.gz --strip-components=1 -C "$dir"
```

**Headlamp in a cluster:** give its container a plugins directory (`-plugins-dir`), filled
by an init container. For example, with the Headlamp chart:

```yaml
config:
  pluginsDir: /headlamp/plugins
initContainers:
- name: mnemo-plugin
  image: busybox
  command: [sh, -c, "mkdir -p /plugins/mnemo-headlamp && wget -qO- $URL | tar -xz --strip-components=1 -C /plugins/mnemo-headlamp"]
  env: [{name: URL, value: "https://github.com/guipguia/mnemo/releases/download/v0.3.0/mnemo-headlamp-0.3.0.tar.gz"}]
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

```sh
cd headlamp
npm ci
npm start   # rebuilds into Headlamp's plugins folder on change
npm run lint && npm run tsc && npm test && npm run build
```
