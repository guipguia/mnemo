# Security Policy

## Reporting

**Do not open public issues for vulnerabilities.**

Use GitHub private vulnerability reporting: *Security → Report a vulnerability* on this repo.
We acknowledge within 3 business days and aim to fix critical issues within 30 days.

## Scope

mnemo stores historical copies of cluster objects. Of particular concern:
- Secret data leaking into stored history or the UI (Secrets are excluded by default).
- Access bypass: users seeing history outside their policy scope, or forging identity
  (OIDC session, auth-proxy headers). See [docs/auth.md](docs/auth.md).
- Over-broad controller permissions.

## Supported versions

Only the latest minor release receives fixes until 1.0.
