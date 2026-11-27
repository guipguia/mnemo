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
  must equal the cluster in each request, and it must allow client auth, or the hub answers 403.
- The main port (`--listen`) then refuses pushes, so mTLS can't be skipped. The token is still
  checked on the agent listener.
- Without mTLS, anyone with the shared token can push as any cluster, and can also replace a
  cluster's agent run (as a restarted agent does), making the real agent stop until it
  restarts. mTLS limits both to the cluster named in the certificate.
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

## Issuing certificates

The guarantee is only as strong as who can get a certificate naming a cluster. **Keep the CA's
private key on the hub side**: anyone who can use it, or create a cert-manager `Certificate`
against an issuer that holds it, can issue `commonName: staging` and push as staging. Use a
CA dedicated to mnemo, and give each agent cluster only its own `tls.crt`, `tls.key` and the
CA's `ca.crt`.

With cert-manager, issue every certificate on the hub cluster from one CA `Issuer`, then copy
each agent's Secret to its cluster (or have your secrets tooling do it):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: mnemo-agents, namespace: mnemo}
spec:
  secretName: mnemo-agents-tls          # hub.agentListener.tlsSecret
  dnsNames: [mnemo-agents.example.com]
  usages: [server auth]
  issuerRef: {name: mnemo-ca, kind: Issuer}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: mnemo-agent-prod, namespace: mnemo}
spec:
  secretName: mnemo-agent-prod-tls      # copy to cluster prod; there, agent.tls.secret
  commonName: prod                      # = agent.cluster there
  usages: [client auth]                 # required: the hub refuses certificates without it
  issuerRef: {name: mnemo-ca, kind: Issuer}
```

Renewals then have to reach the agent cluster too; the agent reloads the files when they
change. To issue on the agent clusters instead, use an issuer that can restrict the name each
cluster may request, such as Vault PKI roles or cert-manager's approver-policy.
