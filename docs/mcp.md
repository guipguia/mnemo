# MCP server

The hub serves [MCP](https://modelcontextprotocol.io) at `/mcp` (streamable HTTP), so AI
incident agents can ask "what changed before this alert?" as tool calls. It only reads, and it
sees what the [access policy](auth.md) lets the caller see, exactly like the UI and API.

| Tool | Answers |
|---|---|
| `list_changes` | The change feed: what changed, when, who, how (gitops / controller / manual), drift, changed fields. Filters by cluster, namespace, kind, author, source, drift, time window. |
| `object_history` | Every version of one object. |
| `object_at` | One object as it was at a time. |
| `list_events` | Archived Kubernetes Events, including a Deployment's rollout. |
| `list_clusters` | Recorded clusters and when their agents last checked in. |

Times are RFC 3339 or a duration ago (`2h`). Secrets are never recorded, so never returned.

## Connect

With `--auth none`, point the client at the hub:

```sh
claude mcp add --transport http mnemo https://mnemo.example.com/mcp
```

Otherwise the client sends what the hub expects, as for the [kubectl plugin](kubectl-plugin.md):

- **`--auth proxy`:** whatever your auth proxy accepts, such as a bearer token for oauth2-proxy
  with `--skip-jwt-bearer-tokens`: `--header "Authorization: Bearer ..."`.
- **`--auth oidc`:** the `mnemo_session` cookie from a browser login,
  `--header "Cookie: mnemo_session=..."`. It expires with `--session-ttl`, and it's your login:
  treat it like a password. MCP's own OAuth flow isn't supported yet.

The agent token (`--token`) is for agents pushing changes and can't read history.
