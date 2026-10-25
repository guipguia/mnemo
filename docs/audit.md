# User attribution from audit events

Out of the box, mnemo knows which **tool** made a change: the field manager in
`managedFields` (`kubectl-edit`, `argocd-controller`, ...). To know which **person** made
it, the API server sends its audit events to the mnemo agent, and the change's author
becomes the audited user (`alice@example.com`, `system:serviceaccount:ci:deployer`).

This needs API server flags, so it works on self-managed clusters (kubeadm, kind, k3s,
Talos, ...). Managed clusters (EKS, GKE, AKS) don't allow audit webhooks; readers for their
cloud audit logs are on the [roadmap](../ROADMAP.md).

## How it works

- The agent receives audit events on `--audit-listen` and keeps successful writes to the
  resources it watches. The rest are dropped on arrival.
- Each one is matched to the version mnemo recorded by its exact `resourceVersion` (deletes:
  by the object's UID). That needs the `RequestResponse` audit level for those writes.
- When someone acts through impersonation (an access proxy such as Teleport or Rancher, or
  `kubectl --as`), the impersonated user is the author.
- The source stays the field manager's (`gitops`, `controller`, `manual`), except that a
  person (not a `system:` identity) is always `manual`, which also flags drift on
  GitOps-managed objects. Deletes, which have no field manager, are classified by the
  client's user agent.
- Best-effort: if the hub is down when events arrive, or an event arrives before the change
  it describes, the field manager stays the author.

## 1. Install the agent with audit on

```sh
kubectl -n mnemo create secret generic mnemo-audit --from-literal=token="$(openssl rand -hex 32)"
helm upgrade --install mnemo oci://ghcr.io/guipguia/charts/mnemo -n mnemo --reuse-values \
  --set agent.audit.enabled=true --set agent.audit.existingSecret=mnemo-audit \
  --set agent.audit.tlsSecret=mnemo-audit-tls \
  --set agent.audit.service.clusterIP=10.96.200.200   # any free IP in your Service CIDR
```

- **`tlsSecret`** (required) is a `kubernetes.io/tls` Secret, e.g. from cert-manager, whose
  certificate is valid for the clusterIP (or node address with a NodePort). The API server
  only sends the token over HTTPS, and audit events include the bodies of watched objects.
- **`service.clusterIP`**: the API server resolves names with the node's DNS, not cluster
  DNS, so it can't use `mnemo-audit.mnemo.svc`. Use a fixed clusterIP (control-plane nodes
  running kube-proxy can reach it) or `service.type=NodePort`.

## 2. Configure the API server

Audit policy: start from [test/e2e/audit/policy.yaml](../test/e2e/audit/policy.yaml) and
merge it into yours. It logs writes to the default watched resources at `RequestResponse`,
never Secrets. The policy applies to every audit backend, so those bodies also reach your
audit log if you have one.

Webhook config (a kubeconfig file):

```yaml
apiVersion: v1
kind: Config
clusters:
  - name: mnemo
    cluster:
      server: https://10.96.200.200:8443
      certificate-authority: /etc/kubernetes/audit/mnemo-ca.crt
users:
  - name: kube-apiserver
    user: {token: <the token from the mnemo-audit Secret>}
contexts:
  - name: mnemo
    context: {cluster: mnemo, user: kube-apiserver}
current-context: mnemo
```

kube-apiserver flags:

```
--audit-policy-file=/etc/kubernetes/audit/policy.yaml
--audit-webhook-config-file=/etc/kubernetes/audit/webhook.yaml
```

Keep the default `--audit-webhook-mode=batch`: it never blocks API requests while mnemo
is down. With kubeadm, set these under `apiServer.extraArgs` with an `extraVolumes` entry
for `/etc/kubernetes/audit`; [test/e2e/kind.yaml](../test/e2e/kind.yaml) is a working
example for kind.

## Security

- The token is what stops anyone in the cluster from posting fake events that name the
  wrong person: keep it secret and rotate it with the API server config.
- Audit events can contain the full bodies of watched objects. mnemo only keeps who made
  each write; the bodies come from its own watch, as before.
