# Agent ↔ hub mutual TLS

By default agents push over the hub's main port with the shared token, and TLS is whatever
sits in front of the hub (usually the ingress). With mutual TLS each agent also proves which
cluster it is: its client certificate must name the cluster it pushes for, so a leaked token
or a compromised agent can't write another cluster's history.

## Hub

```sh
mnemo hub --token ... --agent-listen :8444 \
  --agent-tls-cert hub.crt --agent-tls-key hub.key --agent-ca agents-ca.crt
```

- `--agent-listen` serves only the agent endpoints, over TLS, and requires a client
  certificate signed by `--agent-ca`. The certificate's common name or one of its DNS names
  must equal the cluster in each request, or the hub answers 403.
- The main port (`--listen`) then refuses pushes, so mTLS can't be skipped. The token is still
  checked on the agent listener.
- TLS has to reach the hub intact: expose the port with a LoadBalancer or TLS passthrough,
  not an ingress that terminates TLS.

Chart: `hub.agentListener.enabled` and `hub.agentListener.tlsSecret`, a Secret with
`tls.crt`, `tls.key` and `ca.crt` (the CA for agent certificates). It adds the Service
`<release>-agents` on port 8444, a LoadBalancer by default.

## Agent

```sh
mnemo agent --cluster prod --hub https://mnemo-agents.example.com:8444 --token ... \
  --hub-ca hub-ca.crt --client-cert prod.crt --client-key prod.key
```

Chart: `agent.tls.secret`, a Secret with `tls.crt` and `tls.key` (the client certificate,
naming `agent.cluster`) and `ca.crt` (the CA to trust for the hub), and point
`agent.hubURL` at the agent listener.

Certificates and keys are reloaded when their files change, so renewing in place (as
cert-manager does) needs no restart. CA files are read at startup.

## With cert-manager

One CA for the hub and its agents keeps `ca.crt` the same everywhere:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: mnemo-agents, namespace: mnemo}     # on the hub cluster
spec:
  secretName: mnemo-agents-tls                       # hub.agentListener.tlsSecret
  dnsNames: [mnemo-agents.example.com]
  usages: [server auth]
  issuerRef: {name: mnemo-ca, kind: Issuer}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: mnemo-agent, namespace: mnemo}      # on cluster "prod"
spec:
  secretName: mnemo-agent-tls                        # agent.tls.secret
  commonName: prod                                   # = agent.cluster
  usages: [client auth]
  issuerRef: {name: mnemo-ca, kind: Issuer}          # the same CA, available there
```
