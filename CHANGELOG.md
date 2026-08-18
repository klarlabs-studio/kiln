# Changelog

All notable changes to kiln are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and kiln adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`tasks:` — the automation a pipeline needs that is neither a check nor an
  artifact.** Named commands routed by event (`pull_request`, `push`, `tag`,
  `schedule`), each posting its own `Kiln / <task>` check so branch protection
  can require one and a red check names the thing that broke.

  The line this does not cross: **a task cannot mint provenance.** The signed
  artifacts of a run are exactly what `publish:` produced, so growing this
  surface can never dilute the claim kiln exists to make. `.warden.yaml`
  remains the only check language; this is automation, and it is deliberately
  the weakest of the three things a pipeline does.

  Tasks run after publish, in a disposable worktree pinned to the commit —
  never the operator's working copy — with the environment scrubbed on an
  untrusted head, exactly as the gate runs. One failure does not stop the
  others: artifacts are a set, tasks are independent errands, and hiding the
  second problem behind the first helps nobody. `allow_failure` records a
  failure without failing the run, and concludes the check *neutral* rather
  than red, because a red check for something declared advisory is how a wall
  of red gets ignored.

  Scheduled tasks fire from the watch tick, against the head of the tracked
  ref, with the last run recorded beside the ledger so an interval survives a
  restart. A box that was off for a week fires each due task **once** when it
  comes back — cron catch-up is how a nightly remediation job opens seven pull
  requests at breakfast. A task is marked fired *before* it runs, so one that
  takes the process down does not re-fire on every restart.

  Commands run under `sh -euc`: `-e` so a script that fails halfway fails
  rather than reporting the status of its last echo, `-u` so a typo'd variable
  is an error instead of an empty string deleting the wrong directory.

- **`kiln verify --policy` — verification you can adopt without adopting
  kiln.** Producing provenance means changing how you build; checking it does
  not. The policy file declares whose signature counts, which builders are
  acceptable and whose source verdict is required, so the rules live in a
  reviewable file in the consumer's repository rather than in whatever flags
  somebody typed into a pipeline step. A flag that contradicts the policy is a
  usage error, and a misspelled field is a load error rather than a rule that
  quietly checks nothing.

  `provenance.builders` is what makes it general: a GitHub Actions workflow
  identity is as valid a builder as kiln's own, so an artifact kiln never
  touched verifies with the same command and the same report.

- **The source verdict is verified from the artifact, against the gate's own
  key.** With `source.keys`, kiln checks warden's DSSE envelope with ed25519
  directly — cosign fetches it, cosign does not judge it, because
  `verify-attestation` would check the signature of whoever *attached* the
  summary rather than the gate that made it. No clone of the repository and no
  `warden` binary on the verifying machine. Every configured key is tried
  rather than the one the envelope names: a DSSE `keyid` is
  attacker-controlled metadata, useful for picking a key out of a roster and
  worthless as an authorisation.

- **`kiln doctor` checks registry credentials before a build, not after.**
  `image:` has always accepted any registry — Docker Hub, ECR, Harbor, one on
  your own box — but the push is the last thing a publish does, so a missing
  `docker login` surfaced only after the gate had run and the image had been
  built, and repeated every tick on an unattended box. Doctor now reads
  docker's own configuration and says so up front.

  Docker Hub in particular: docker records it as `https://index.docker.io/v1/`
  and always has, so a check looking up "docker.io" would report a missing
  login that is right there. A credential store or helper counts as present —
  holding credentials outside that file is its entire job — and an
  unreadable or absent config is reported as *unknown* rather than missing,
  because a CI runner may inject credentials in a way this cannot see.

- **`kiln verify --json`** emits the report for a job that has to act on which
  link broke. The exit code is unchanged and the message goes to stderr, so
  stdout stays parseable.

### Fixed

- **A `go install`ed kiln knows what it is.** Installing from the module path
  passes no ldflags, so the binary reported `kiln dev (unknown) built unknown`
  — leaving the operator of a provenance tool unable to say which provenance
  tool they were running. The module version and VCS stamp the toolchain
  already embeds are read when the link-time values are absent. A release's
  own ldflags still win.

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
