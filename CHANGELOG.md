# Changelog

All notable changes to mnemo, generated from commit messages by [git-cliff](https://git-cliff.org).

## Unreleased

### Added

- Add a retention policy (age / per-object count) ([#20](https://github.com/guipguia/mnemo/pull/20))

### Changed

- Stop tracking the mnemo binary ([#19](https://github.com/guipguia/mnemo/pull/19))

## [v0.1.0](https://github.com/guipguia/mnemo/releases/tag/v0.1.0) - 2026-10-21

### Added

- Add SQLite store for object history and change feed
- Add agent normalization and hub resume protocol
- Add agent watch loop, mnemo binary, and field-ownership attribution
- Add the incident demo and README GIF ([#14](https://github.com/guipguia/mnemo/pull/14))
- Add Kyverno policies that refuse unsigned mnemo images ([#16](https://github.com/guipguia/mnemo/pull/16))
- Add an end-to-end test on kind, run in CI ([#18](https://github.com/guipguia/mnemo/pull/18))

### Changed

- store: add drift flag, cluster registry, checkpoints, and prune
- hub: add ingest and read API
- Web UI: change feed, object timeline, diffs, state at time T ([#7](https://github.com/guipguia/mnemo/pull/7))
- SSO: OIDC login, auth-proxy headers, per-user access policy ([#8](https://github.com/guipguia/mnemo/pull/8))
- Packaging: container image, Helm chart, read-only RBAC from config ([#9](https://github.com/guipguia/mnemo/pull/9))
- Record when changes happened, not when they were processed ([#10](https://github.com/guipguia/mnemo/pull/10))

### Fixed

- Fix 6 dependency CVEs; scan images in CI and before release ([#15](https://github.com/guipguia/mnemo/pull/15))

### Tests

- Test the agent watch loop and the hub's route wiring ([#17](https://github.com/guipguia/mnemo/pull/17))

### CI and docs

- docs: describe field-ownership attribution and offline catch-up
- ci: pin GitHub Actions to commit SHAs ([#6](https://github.com/guipguia/mnemo/pull/6))
- ci: release workflow with signed binaries, image, and chart ([#11](https://github.com/guipguia/mnemo/pull/11))

### Dependencies

- Bump ossf/scorecard-action from 2.4.0 to 2.5.0
- Bump actions/checkout from 4 to 7
- build(deps): bump actions/checkout from 4 to 7
- build(deps): bump actions/setup-go from 5 to 7
- dependabot: don't propose Node major versions for the Dockerfile ([#13](https://github.com/guipguia/mnemo/pull/13))

