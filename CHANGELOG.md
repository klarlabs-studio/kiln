# Changelog

All notable changes to kiln are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and kiln adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-08-18

First release. Kiln takes a commit warden has already gated, builds it, signs
what comes out, and attaches enough evidence that nobody downstream has to
trust the machine it ran on.

### Added

- **Prove.** `kiln run --sha S --event E` runs warden's gate on a pinned,
  disposable worktree — never the operator's working copy, because an
  uncommitted edit sitting in a checkout would otherwise end up inside a signed
  image. A commit carrying a note signed by a key the operator pinned in
  `KILN_TRUSTED_KEYS` may skip the re-prove; without pinned keys there is no
  skip, since `warden verify` with no `--key` would validate a note signed by
  anyone, including the pull request author.

- **Publish, as a typed list.** `publish:` takes artifacts of `kind: image`
  (docker build, push, `cosign sign`) and `kind: binaries` (delegated to
  GoReleaser). Kiln reads `.goreleaser.yaml` before building and **refuses a
  release config with no `signs:` block**, so a verifiable release does not
  depend on whether somebody remembered.

- **A provenance chain with two authorities.** Kiln attaches SLSA v1 build
  provenance signed with its own cosign identity, and carries warden's signed
  Verification Summary Attestation **without re-signing it**. A summary
  re-signed by the builder would attest to the builder; carried intact, the
  source verdict stands on warden's key alone. Kiln refuses an unsigned
  summary, a `FAILED` verdict, and one whose commit is not the commit it built.

- **`kiln verify <ref>`** walks a published artifact's whole chain — signature,
  provenance, builder identity, source gate — and reports which links are
  established rather than a single yes.

- **Isolation as a function of the event.** Fork pull requests never inherit a
  skip and never publish. The caller states intent; the policy decides.

- **Unattended operation.** `kiln watch --every D`, or `--repos /srv/*` for a
  fleet from one process. An `flock` per repository so overlapping cron ticks
  serialise instead of racing, a phase timeout so a hung `docker pull` cannot
  pin a watcher, a reaper for the worktrees killed runs leave behind, and a
  local docker prune bounded to the images this pipeline publishes — never a
  registry, because a deleted digest is a rollback RollOps can no longer
  perform.

- **Four surfaces over one engine.** CLI, MCP (read-only by default; push and
  tag runs refused unless `KILN_MCP_ALLOW_RUN=1`), optional `kilnd` HTTP with
  bearer auth and HMAC webhooks, and GitHub Checks as the human UI. There is no
  deploy tool on any of them, and adding one would be a category error.

- **Exit codes cron can act on:** `2` the gate rejected the change, `3`
  configuration or toolchain, `75` another kiln holds the repository. "Your
  code is wrong" and "this machine is broken" are different pages.

### Notes

Kiln is deliberately not an Actions clone, not a `runs-on` worker and not CD.
Where it loses to the alternatives, and to whom, is written down in
[`docs/competitive.md`](docs/competitive.md) rather than left for an operator
to discover.

[Unreleased]: https://github.com/klarlabs-studio/kiln/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/klarlabs-studio/kiln/releases/tag/v0.1.0
