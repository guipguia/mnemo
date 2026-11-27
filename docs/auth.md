# Authentication and access

mnemo stores the history of every watched object in every cluster, so the UI and read API
need two things: **who** is asking (login), and **what** they may see (policy).
Agents don't use either: they authenticate to the hub with `MNEMO_TOKEN`.

| `--auth` | Who is asking | Use when |
|---|---|---|
| `none` (default) | Nobody checks | Local use, `kubectl port-forward`. The hub warns if it listens on a non-loopback address. |
| `oidc` | mnemo runs the OIDC login itself | You have an IdP: Okta, Microsoft Entra ID, Google, Keycloak, Auth0, Dex, ... |
| `proxy` | An auth proxy in front of mnemo sets user/group headers | You already run oauth2-proxy or an ingress with auth |

With `oidc` or `proxy`, `--policy` is required. Without a matching policy rule, a logged-in
user sees nothing.

## Policy

```yaml
rules:
# Full access: every cluster, every namespace, and cluster-scoped objects.
- groups: [sre]

# A team: two clusters, its own namespaces only.
- groups: [team-shop]
  users: [alice@example.com]      # users and groups can be mixed
  clusters: [prod, staging]       # omit for every cluster
  namespaces: [shop, "shop-*"]    # omit for every namespace AND cluster-scoped objects
```

- Patterns are a name, `"*"`, or `"prefix*"`.
- A user matching several rules sees the union.
- Objects without a namespace (ClusterRoles, ClusterRoleBindings, Namespaces, ...) are only
  visible from rules with **no** `namespaces:` list. `namespaces: ["*"]` means "every
  namespace", not "everything".
- Unknown keys are errors, so a typo like `namespace:` stops the hub instead of silently
  granting a whole cluster.
- Objects outside your access look exactly like objects that don't exist (empty history, 404).

### RBAC mode

Instead of (or as well as) listing teams in the policy, let each cluster's own Kubernetes
RBAC decide: a user sees, kind by kind, what the cluster lets them `get`, `list` or `watch`.

```yaml
rbac:                     # `rbac: {}` with no prefixes; a bare `rbac:` leaves it off
  userPrefix: "oidc:"     # the API server's --oidc-username-prefix, if any
  groupPrefix: "oidc:"    # its --oidc-groups-prefix
rules:                    # optional: rules still add to what RBAC grants
- groups: [sre]
```

The hub has no credentials for your clusters, so it evaluates the RBAC objects the agents
record (Roles, ClusterRoles, RoleBindings, ClusterRoleBindings: watched by default). For a
user `alice` in group `team-shop`, a RoleBinding in `shop` binding `User oidc:alice` or
`Group oidc:team-shop` to the `view` ClusterRole shows them the kinds `view` can read in
`shop`, and nothing else.

- **Identities must match.** The username and groups from your login (`--oidc-username-claim`,
  the proxy headers) plus the prefixes must be the names your bindings use. With EKS IAM,
  GKE Google accounts and similar, they often aren't: check a few users on the
  "Can see" line in the UI header.
- About 30 seconds stale: the hub reloads the recorded RBAC in the background every 30
  seconds, so a revoked binding takes that long to apply (plus however long the agent takes
  to see it).
- With OIDC, the session keeps only the groups that a policy rule or a recorded binding
  names at login. A binding for a group added later applies at the user's next login.
- Narrower than Kubernetes: rules limited to `resourceNames`, subresources, ServiceAccount
  subjects and implicit groups (`system:authenticated`) grant nothing. Users and groups
  whose name (with its prefix) starts with `system:` are ignored, as the API server reserves
  those: an IdP group called `system:masters` must not mean cluster-admin.
- Events are shown with the object they're about, so you need access to that object's kind.
- Kinds are matched to resource names by the usual plural (`NetworkPolicy` →
  `networkpolicies`). A custom resource with an irregular plural won't match.

## OIDC

Register mnemo as a web application (confidential client) in your IdP with the redirect URL
`https://<mnemo host>/auth/callback`, then:

```sh
export MNEMO_OIDC_CLIENT_SECRET=...
export MNEMO_SESSION_KEY=$(head -c 48 /dev/urandom | base64)   # keep stable across restarts

mnemo hub --auth oidc --policy /etc/mnemo/policy.yaml \
  --oidc-issuer https://idp.example.com \
  --oidc-client-id mnemo \
  --oidc-redirect-url https://mnemo.example.com/auth/callback \
  --oidc-scopes openid,email,profile,groups
```

| Flag | Default | |
|---|---|---|
| `--oidc-username-claim` | `email` | Logins with `email_verified: false` are refused. |
| `--oidc-groups-claim` | `groups` | Only groups that appear in the policy are kept in the session. |
| `--session-ttl` | `8h` | Sessions are signed cookies (HttpOnly, SameSite=Lax, Secure on https). |

The login uses the authorization code flow with PKCE, state, and nonce.

### Provider notes

**Okta.** Add a `groups` claim to the authorization server (filter e.g. `Matches regex .*`)
and request the `groups` scope.

**Microsoft Entra ID.** Issuer `https://login.microsoftonline.com/<tenant-id>/v2.0`. Under
*Token configuration*, add a groups claim. Entra sends group **object IDs**, not names, so use
the IDs in the policy. For users in many groups, emit only "groups assigned to the
application". Users without a mailbox may have no `email`: use
`--oidc-username-claim preferred_username`.

**Google.** Issuer `https://accounts.google.com`. Google ID tokens carry no groups: use
`users:` in the policy, or put Dex in front to get Google Groups.

**Keycloak.** Issuer `https://<keycloak>/realms/<realm>`. Add a *Group Membership* mapper
named `groups` with *Full group path* off.

**GitHub, GitLab, LDAP, SAML.** These aren't OIDC providers for user login (or need
translation). Run [Dex](https://dexidp.io) and point mnemo at it. Dex's GitHub connector
reports teams as `org:team` groups.

## Auth proxy

mnemo reads the user and comma-separated groups from headers, **only** on connections from
`--proxy-trusted-cidrs`. Anything else gets 401, so a client that reaches the hub directly
can't claim to be someone else.

```sh
mnemo hub --auth proxy --policy /etc/mnemo/policy.yaml \
  --proxy-trusted-cidrs 10.0.0.0/8 \
  --proxy-user-header X-Forwarded-User \
  --proxy-groups-header X-Forwarded-Groups
```

With oauth2-proxy as a reverse proxy, `--pass-user-headers` sets `X-Forwarded-User` and
`X-Forwarded-Groups`. With nginx `auth_request` and `--set-xauthrequest`, use
`X-Auth-Request-User` and `X-Auth-Request-Groups`.

Keep the trusted range as narrow as possible (the proxy's pods or nodes), and make sure the
proxy strips these headers from incoming client requests. **Never include the Kubernetes API
server's addresses** (or a node range that covers them): its service proxy
(`/api/v1/namespaces/.../services/mnemo:http/proxy/...`) passes client headers through, so
anyone allowed `services/proxy` on the hub could claim to be any user.
