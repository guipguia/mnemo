# Contributing

Thanks for helping. Small PRs merge fast; large ones start as an issue.

## Workflow

1. Open or pick an issue. Comment that you're on it.
2. Fork, branch, change, add a test.
3. Sign off every commit (DCO): `git commit -s`.
4. Open a PR. One maintainer approval + green CI merges it.

## Developer Certificate of Origin

We use the [DCO](https://developercertificate.org/), not a CLA. The `-s` flag adds
`Signed-off-by: Your Name <you@example.com>`, certifying you have the right to submit the work.
Forgot it? `git rebase --signoff main && git push -f`.

## Design changes

Anything that changes the storage format, CRDs, or public API needs a short proposal
in `docs/proposals/` first, discussed in a PR.

## Development

Needs Go (see `go.mod`), Node 24, Docker, and [kind](https://kind.sigs.k8s.io/).

```sh
# A throwaway cluster with its own kubeconfig, so your current context is untouched.
kind create cluster --name mnemo-dev --kubeconfig /tmp/mnemo-dev.kubeconfig

# Build the UI into the binary, then run hub + agent in one process.
(cd ui && npm ci && npm run build)
go build -o bin/mnemo ./cmd/mnemo
./bin/mnemo all --cluster dev --kubeconfig /tmp/mnemo-dev.kubeconfig --listen 127.0.0.1:8080
```

Open http://127.0.0.1:8080, then change something with
`KUBECONFIG=/tmp/mnemo-dev.kubeconfig kubectl ...` and watch it appear.

For UI work, keep the hub running and use `cd ui && npm run dev`: Vite serves the UI with
hot reload and proxies `/api` to the hub on port 8080.

Tests: `go test -race ./...` and `cd ui && npm run build` (typechecks).

## Where to start

Issues labelled `good first issue`.
