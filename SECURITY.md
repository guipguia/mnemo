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

## Verifying releases

Release artifacts are signed keylessly with [Sigstore](https://www.sigstore.dev): the
signature proves they were built by this repository's release workflow from a tag.

```sh
ID='^https://github.com/guipguia/mnemo/.github/workflows/release.yml@refs/tags/v'
ISSUER=https://token.actions.githubusercontent.com

# Container image and Helm chart
cosign verify ghcr.io/guipguia/mnemo:v0.1.0 --certificate-identity-regexp "$ID" --certificate-oidc-issuer "$ISSUER"
cosign verify ghcr.io/guipguia/charts/mnemo:0.1.0 --certificate-identity-regexp "$ID" --certificate-oidc-issuer "$ISSUER"

# Binaries: verify the checksums file, then the archive against it
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json --certificate-identity-regexp "$ID" --certificate-oidc-issuer "$ISSUER"
sha256sum --ignore-missing -c checksums.txt

# Build provenance (SLSA), with the GitHub CLI
gh attestation verify mnemo_v0.1.0_linux_amd64.tar.gz --repo guipguia/mnemo
```

Images also carry an SBOM: `docker buildx imagetools inspect ghcr.io/guipguia/mnemo:v0.1.0 --format '{{ json .SBOM }}'`.
