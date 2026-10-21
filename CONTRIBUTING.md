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

## Where to start

Issues labelled `good first issue`.
