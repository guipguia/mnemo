# mnemo

A time machine for Kubernetes cluster state.

> What did this Deployment / ConfigMap / RoleBinding look like at 14:32 yesterday, and who changed it?

Audit logs are raw and noisy. GitOps only covers what lives in Git. mnemo watches live
resources across all your clusters, stores compact diffs, and answers *"what changed in the
last 30 minutes?"*, flagging manual changes that bypassed GitOps and correlating them with
Events and rollouts.

<!-- TODO: demo GIF of the "broken ConfigMap" incident -->

**Why mnemo:** open source, self-hosted, your data never leaves your infrastructure, and you
can reconstruct any object at any point in time, not just get notified that it changed.

**Status:** early design. Nothing to install yet. See [ROADMAP.md](ROADMAP.md).

## How it works

- **Agent (Go, one per cluster):** watches configured resource kinds, strips noise
  (`status`, `resourceVersion`, `managedFields`), pushes diffs to a central hub.
- **Hub:** stores history for all clusters (SQLite first; PostgreSQL and MySQL planned)
  and serves the API and UI. Single-cluster installs run both in one pod.
- **Attribution:** an optional audit webhook backend supplies *who* made each change
  (`watch` alone only tells you *what*). See [docs/design.md](docs/design.md).
- **UI (React):** timeline per object, side-by-side diffs, overlay of Events and rollouts.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). All commits require a DCO sign-off (`git commit -s`).

## Community

- Code of Conduct: [CNCF Code of Conduct](CODE_OF_CONDUCT.md)
- Governance: [GOVERNANCE.md](GOVERNANCE.md)
- Security issues: [SECURITY.md](SECURITY.md), never public issues

## License

[Apache 2.0](LICENSE)
