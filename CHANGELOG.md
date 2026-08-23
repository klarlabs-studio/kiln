# Changelog

All notable changes to kiln are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and kiln adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **The release workflow runs the test suite in the environment CI runs it
  in.** `release.yml` grants `id-token: write` so cosign can sign keylessly,
  and a workflow-level permission reaches every job — which sets
  `ACTIONS_ID_TOKEN_REQUEST_URL`, a variable CI never sets. A test that reads
  the ambient environment therefore passed locally, on pull requests and on
  `main`, and failed only once a tag was pushed. That is the worst moment to
  find out, and it cost two tags: `v0.3.0` and `v0.3.1` are tagged with no
  release attached.

  The suite now runs in its own job that overrides permissions down to
  `contents: read`, with the release job depending on it. The tag's own commit
  is still tested — a tag can be moved, and that is the last check before
  artifacts exist that anyone will trust — but in the same environment that
  vouched for it on `main`.

  A test asserts it, because the alternative is a comment in a YAML file that
  nothing reads.

### Fixed

- **`brew upgrade` locked every box out of its own token.** `kiln login` put
  the running binary on the keychain item's trusted-application list after
  resolving it through `EvalSymlinks` — under Homebrew,
  `Caskroom/kiln/<version>/kiln`. Upgrading deletes that, so the next build is
  not on the list, and a background schedule cannot read the token: it needs an
  approval dialog it has no way to answer. The box then runs credential-less,
  which means no commit statuses and every pull request treated as a fork,
  announced by one `warn` line.

  This is the same mistake as the launchd plist, in a second place, so the
  answer now lives in one place — `binpath.Stable`, used by both. The access
  list names the path the package manager repoints, and the resolved binary as
  well when they differ.

## [0.4.0] - 2026-08-23

### Added

- **`prove.materialize`, so a box can gate a Node project at all.** A gate runs
  in a disposable worktree and `node_modules` is gitignored, so it was in the
  clone and never in the checkout. Every Node repository's gate failed with
  `vitest: command not found` — while passing by hand in the clone, which
  reads as flakiness rather than a missing file. Go repositories never noticed,
  because their dependencies come from the module cache, which is global.

  ```yaml
  prove:
    from: warden
    materialize: [node_modules]
  ```

  Entries are hardlinked from the clone into the worktree before the gate
  runs, never over a path the commit itself provides, and a named directory
  that is absent is skipped rather than failing the run.

  **A fork materialises nothing.** The list is read from the commit being
  gated, so on a pull request its author wrote it; `materialize: [".env"]`
  would otherwise hand a stranger whatever sits beside the operator's clone,
  inside code kiln is about to execute. It is governed by the same bit as
  registry and signing credentials, so a fork's gate fails with "could not
  run" — which is the honest outcome, and means a Node repository still cannot
  be gated on fork pull requests.

### Fixed

- **`kiln box status` reported a healthy box while the schedule was dead.**
  The check added in 0.3.5 stat'd the binary kiln was running rather than the
  one the installed unit names. After an upgrade those differ — the running
  one is fine and the scheduled one has been deleted — which is precisely the
  case it existed to catch, so it reported "last tick wrote ..." about a box
  that had not run since.

  It reads the program out of the unit now, anchored on the
  `ProgramArguments` key: scanning for the first `<string>` in a plist returns
  the *label*, which is not a file, so the check appeared to work while naming
  the wrong thing.

## [0.3.5] - 2026-08-23

### Fixed

- **`brew upgrade` silently stopped every installed box.** `kiln box install`
  resolved the running binary through `EvalSymlinks` and wrote the result into
  the schedule, which under Homebrew is
  `/opt/homebrew/Caskroom/kiln/<version>/kiln`. Upgrading deletes that
  directory. The job stayed loaded, `launchctl list` still showed it, and
  nothing ran again — no log line, no status, no non-zero exit to alert on.

  A box exists so nobody has to remember to check, so one that stops quietly
  is worse than no box: the operator believes commits are being gated when
  they are not. It happened to a real box here the moment 0.3.4 landed.

  The schedule now names the stable path the package manager repoints —
  `/opt/homebrew/bin/kiln` — and only when it resolves to the same binary
  that is running, so a box installed from a local build is never quietly
  scheduled against a different kiln.

  `kiln box status` also reports a schedule whose binary has gone, since that
  is the check which would have caught this without anyone going to look.

## [0.3.4] - 2026-08-23

### Fixed

- **kiln reported "gate failed" when warden said it could not run the gate.**
  Every non-zero exit from warden became `ErrGateFailed`, so a missing
  toolchain was posted as a red `Kiln / Prove — gate failed` on a commit
  nothing had looked at. Observed on a real box, on a healthy `main`.

  Warden's exit codes are a sysexits(3) contract: `1` is a rejection, `75`
  (`EX_TEMPFAIL`) is contention, `78` (`EX_CONFIG`) is a step whose toolchain
  is not installed. kiln now reads them, and reports the last two as
  `ErrToolMissing` and `ErrGateUnavailable` — with exit code 3, not 2, since
  claiming a change was rejected is a claim nothing there is entitled to make.

  The check stays a *failure* conclusion, deliberately. A commit whose gate
  never ran has not been gated, and a neutral conclusion posts as a success
  commit status, which would show an unverified commit green. What changes is
  the claim, not the colour: the title is now "gate could not run".

  Anything unrecognised is still a gate failure, which is the safe direction —
  an exit code kiln has not been taught about must not excuse a commit.

### Changed

- **The internals are layered.** `internal/` is now
  `domain`, `application`, `infrastructure` and `interfaces`, with `boot` as
  the composition root. `internal/arch` walks every non-test file and fails
  the build if an import points the wrong way, so the layering is checked
  rather than merely intended.

  Nothing users can see changed, and no behaviour with it. It is listed here
  because every internal import path moved: a branch opened before this will
  conflict across most files, and rebasing it is mostly a matter of
  `internal/x` becoming `internal/<layer>/x`.

## [0.3.3] - 2026-08-23

### Fixed

- **A box that lost its token replayed every merged pull request.** The
  tokenless fallback recognised a merged pull request by asking whether its
  head was an ancestor of the watched branch. That only holds for a merge
  commit: a squash or rebase merge writes a new commit, so the head is never an
  ancestor and every merged pull request read as unmerged.

  Found on a real box within the hour. dispatch squash-merges. With a token it
  skipped 42 pull refs and built none; the moment the token went away it
  started gating #31, which had merged. The token went away because
  `brew upgrade` moved the binary to a new path and the keychain ACL still
  trusted the old one — a box has no way to answer the approval dialog that
  follows, so it silently degrades to no token.

  The baseline now records every pull ref a repository already had, not just
  the ones a token said were open, and a tick that could not ask GitHub falls
  back to it. With a token nothing changes: the API is still the authority, and
  a pull request open at install time is still gated on every tick. Without
  one, a box gates a pull request as soon as anybody pushes to it — the head
  moves, so the baseline stops covering it — which is the most a box that
  cannot post a commit status could usefully do anyway.

## [0.3.2] - 2026-08-23

0.3.0 and 0.3.1 were tagged but never published, and no artifacts exist for
either. Both release builds died on a doctor test that read the runner's
environment instead of describing its own: `release.yml` grants
`id-token: write` so cosign can sign keylessly, which means an OIDC identity is
present there and nowhere else the test had ever run. The test asserts the
warning doctor gives a box with *no* identity, so it passed locally, in CI and
on `main`, and failed only once a tag was pushed — the one moment the mistake
is expensive. Fixed in #33; the environment gap that hid it is #34.

Both tags are left where they are. Quietly re-pointing a public ref to tidy up
is worse than explaining it.

### Added

- **`KILN_COSIGN_KEY` — keyed cosign signing.** Kiln passed it to cosign's
  `--key` for the signature and the provenance attestation alike, so both
  claims about an artifact are made by the same signer. Any form cosign
  accepts: a key file, `env://VAR`, `k8s://ns/name`, or a KMS URI. Empty keeps
  keyless, unchanged.

  This is what makes a self-hosted builder work. Keyless needs an ambient OIDC
  identity to prove; with none, cosign falls back to the browser device flow —
  which is a hang, not an error. A real tag failed this way after 22 minutes,
  publishing one of six images before the device code expired, and the tag had
  to be repaired by hand.

- **`kiln doctor` reports the signing mode**, and warns for the two
  configurations that hang rather than fail: keyless with no OIDC identity in
  the environment, and a key file with `COSIGN_PASSWORD` unset. Both are
  invisible until a release is already half-published, which is why doctor is
  the place to say it.

### Fixed

- **`kiln doctor` failed every tag-only pipeline.** It planned each artifact
  against whatever ref is checked out — a branch, normally — even for an
  artifact declared `on: [tag]`, which can never be built from one. For the
  common `tags: [sha, semver]` that produced "no moving tag ... publish semver
  only on tag events", advising the operator to do exactly what the config
  already said. On senat-os that was six failures and a non-zero exit on a
  correct pipeline, which is worse than no check: the one signal before a
  release was entirely noise, and it was the run that should have reported the
  signing problem. A tag-only artifact is now planned against a tag, labelled
  as hypothetical when doctor is not standing on one. A branch build that
  really has no moving tag still fails, and a test pins that.

- **A fresh box republished every release the repository had ever cut.**
  Nothing bounded tag discovery to tags that appeared *since* the box was
  installed, and a new box has an empty ledger, so every tag on the remote
  looked like new work. A tag is a publishing event, so this did not merely
  re-gate history — it pushed images and wrote fresh provenance for versions
  that were signed long ago. senat-os has 133 tags.

  A box now records the tags a repository already had on its first tick and
  builds only what happens after. The record lives in `.kiln/baseline.json`,
  beside the run ledger rather than inside it: seeding the ledger with
  synthetic successes would have made `kiln box runs` list runs that never
  happened, and the ledger is the evidence trail. A tag later moved to a
  different commit is built again, because the artefact it would publish is
  not the one that was recorded.

  Only tags are baselined. The branch tip and any open pull requests are
  current work, and building them is both the point of installing a box and
  the only sign the operator gets that the pipeline runs at all. A box that
  already has run history is not given a baseline, since it has built its tags
  already and a baseline written underneath it could silence a tag that is
  mid-backoff after a real failure.

  Together with the closed pull request fix, a first tick on senat-os goes
  from 523 jobs to 3.

- **A box gated every pull request the repository had ever had.** Discovery
  fetches `+refs/pull/*/head` and built every ref it found. GitHub never
  deletes those refs, so they are not the open pull requests — they are the
  complete history of them. Measured on the repositories here: vorhut had 99
  such refs against 3 open pull requests, senat-os 390 against 2.

  The first tick of a new box therefore re-gated hundreds of long-merged
  commits at minutes apiece — on senat-os, about two days of work — and posted
  a commit status on each one. Discovery now builds a pull ref only while its
  pull request is still open, and says how many it skipped and why. Where
  there is no token to ask with, a head already contained in the watched
  branch has certainly been merged and is skipped on that evidence alone;
  anything still unaccounted for is built, and still fails closed as a fork.

  It shipped because every test created a pull ref and expected a job back,
  which is exactly what an open pull request looks like. Nothing had ever
  described the closed one. Found by installing a box on a real repository and
  reading its first tick: it announced it was building `refs/pull/4/head`,
  which had merged some time ago.

### Changed

- **`docs/operating.md` said GitHub deletes `refs/pull/N/head` when a pull
  request closes.** It does not, and that belief is what produced the bug
  above. The tick description now says what actually happens, and a new
  section describes what a box installed on an existing repository does *not*
  do with that repository's history.

## [0.2.1] - 2026-08-23

### Fixed

- **`kiln login` panicked before it reached the API.** `WhoAmI` built a
  `Client` literal instead of calling `NewClient`, leaving `HTTP` nil and
  `BaseURL` empty, so the first request dereferenced a nil pointer. That is
  step two of the three-command quick start, and it failed on every path —
  interactive and `--with-token` alike. A box could not be set up at all
  without writing the keychain by hand.

  It shipped because `WhoAmI` had no test: every other call site goes through
  the constructor that sets those fields, so nothing exercised the one place
  that did not.

  Note for whoever installs a box: **log in with the same binary the box will
  run.** The keychain item names the permitted reader with `-T`, so a token
  stored by a different build makes every unattended tick block on an invisible
  permission dialog.

## [0.2.0] - 2026-08-22

Putting kiln in front of a real fleet, and fixing what that exposed.

### Added

- **`args:` on `kind: image` — one Dockerfile, several images.** A repository
  routinely builds more than one image from a single Dockerfile, differing only
  by a build argument. Kiln had no way to express that, which meant it could
  not build the repository this was found in: senat-os produces six images, and
  `senat-api`, `senat-runtime` and `senat-voiceprobe` are one Dockerfile
  differing only by `BIN=`.

  A map rather than a list, so an argument given twice is an error YAML catches
  for free. No passthrough form (`--build-arg FOO` taking FOO from the
  environment) on purpose: a build whose output depends on the box's
  environment is not reproducible from the commit, and that reproducibility is
  the whole claim kiln makes about an artifact.

  The arguments are recorded in the provenance, at
  `buildDefinition.externalParameters.buildArgs` — two images from one commit
  and one Dockerfile would otherwise carry identical attestations while their
  contents differ, and whoever reproduces the build needs them. Flags render
  sorted, so the same inputs produce the same command line and the same
  statement.

  On a `kind: binaries` entry `args` is a load error; goreleaser owns that build.

## [0.1.3] - 2026-08-20

Both entries came from installing a box on a real repository and watching what
it did.

### Fixed

- **`kiln box install --every 10m` ignored the flag.** Go's flag package stops
  parsing at the first non-flag argument, so anything written after the verb —
  the natural way to write it — was silently dropped and the default applied.
  The verb is now separated before parsing, so both orders work.

### Added

- **`kiln box install --branches-only`** runs `poll` rather than `watch`: the
  tracked branch, no pull requests, no tags, and no GitHub token needed for
  any of it. Pointing a fresh box at a repository with a dozen open pull
  requests otherwise means gating all of them before it reaches the branch you
  care about, which on a laptop is an afternoon of fans.

### Changed

- **The docs say to give the box its own clone.** Kiln is careful with a shared
  checkout — a detached worktree per phase, a lock between ticks — but it
  cannot keep other people out, and a working copy is where branches get
  checked out and reset. A tick that overlaps one of those gates a commit
  nobody is looking at any more.

## [0.1.2] - 2026-08-19

Two things a box needs before it can be left alone: a pipeline you did not have
to write, and a failure that does not spin.

### Added

- **`kiln init`** writes `.kiln.yaml` from what is already in the repository: a
  Dockerfile becomes an image artifact with the registry path derived from the
  git remote, a `.goreleaser.yaml` becomes a binary release, and a repository
  with neither gets a pipeline that proves and publishes nothing rather than an
  invented artifact. It says what it inferred, refuses to overwrite without
  `--force`, and names a missing `.warden.yaml` out loud — kiln runs no checks
  of its own, so a pipeline without one proves nothing.

  The generated file is parsed by kiln's own loader before it is written. A
  generator that emits something its loader rejects is worse than no generator.

### Fixed

- **A failing commit is no longer re-gated on every tick.** CI runs once per
  push; a watch loop runs every few minutes forever, and nothing in kiln
  distinguished the two. The first real box produced **222 runs, 205 of them
  failures** in an afternoon — re-running `go test -race` across thirteen open
  pull requests every five minutes on a laptop, none of which was ever going
  to pass without somebody pushing a fix.

  A failed commit now waits before its next attempt: fifteen minutes, doubling
  to an hour and staying there. Not "never again", because a failure is often
  not about the commit — a registry down, a dependency yanked, a tool missing
  from the box — and those get fixed without anybody pushing anything. A
  genuine breakage settles into an hourly heartbeat instead of a spin.

## [0.1.1] - 2026-08-19

Everything in this release came from trying to move one real private repository
off GitHub Actions, and most of it from the attempt failing.

### Added

- **`kiln login` and `kiln box install` — two commands between installing kiln
  and having a build box.** The honest instructions used to be a cron line
  with a token in it, or a Kubernetes manifest plus a container image carrying
  your whole toolchain. Both are places people stop, and neither is necessary:
  a box is a machine that already has your tools on it, usually the one you
  are typing on.

  `kiln login` stores the token in the macOS keychain or the freedesktop
  secret service, falling back to a 0600 file and saying so rather than
  pretending. It validates the token before storing it, so a wrong one is
  caught at the moment it is pasted rather than on a scheduled tick nobody is
  reading. `kiln box install` writes a launchd agent or a systemd user timer,
  loads it, and ticks immediately.

  Two traps the installer handles, both found by installing it: a launchd
  agent inherits `/usr/bin:/bin:/usr/sbin:/sbin` and nothing else — the first
  box found thirteen commits and failed all thirteen on a missing `warden` —
  so the unit carries the PATH you installed with; and a background job
  reading the keychain pops a dialog unless the binary is on the item's access
  list, which `kiln login` arranges.

### Fixed

- **Kiln can post results outside GitHub Actions at all.** The Checks API
  refuses anything that is not a GitHub App —
  `403 You must authenticate via a GitHub App` — and inside Actions this is
  invisible, because the `GITHUB_TOKEN` there *is* an app installation token.
  The first time kiln ran against a repository from a box, every check silently
  failed to post, which made the whole "require the Kiln checks in branch
  protection" story impossible with a personal access token.

  Kiln now falls back to commit statuses on that specific 403, once per
  process. Branch protection accepts either as a required context, so the name
  still gates a merge; the body is plainer. `kiln doctor` no longer promises
  check runs it may not be able to create.

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

- **`services:` — containers the gate needs beside it.** The blocker the first
  migration found: skene and vorhut both use Actions service containers, so
  neither could leave. An image, an environment and a readiness probe; the gate
  and every task get `KILN_SERVICE_<NAME>_HOST/PORT`.

  The host port is allocated by docker and read back rather than fixed. A box
  runs many repositories, and two pipelines both binding 5432 would collide in
  a way that reads as a flaky test. Loopback only, since a test database should
  not be reachable from the network. Readiness is polled before the gate
  starts, because a gate that begins before postgres accepts connections fails
  in a way nobody debugs twice. Teardown is guaranteed — after the tasks, on
  failure, on cancellation, and a service that fails to start takes down the
  ones already up before returning.

  **`keep:` on a task** copies declared globs out of the worktree before it is
  destroyed, into `.kiln/runs/<run-id>/<task>/` — the local answer to 22
  upload-artifact uses. Kept on failure too, especially on failure: the log
  that explains a failure is exactly what somebody wants after the tree is
  gone. A glob matching nothing is reported rather than passed over. Patterns
  cannot escape the worktree, including through a symlink, because the pattern
  comes from the repository and retention writes somewhere an operator reads.
  Bounded at the last 20 runs.

  **`pull_request:` on a task** commits what the command changed, pushes the
  branch and opens or updates a pull request — the capability 19 repos depend
  on the shared nox-remediate workflow for. Nothing happens when the worktree
  is clean: an empty commit and a pull request saying "nothing to fix" is how
  an automation becomes noise. One branch, so a daily task updates its own
  pull request rather than opening thirty; labels applied on creation only, so
  an operator who removed one is not fighting the machine every morning; the
  branch rebuilt from the commit under test, so yesterday's fix does not
  outlive the code it was fixing. A failed task proposes nothing. A task routed
  to `pull_request` may not open one — that is a loop with a write credential
  in it — and an untrusted head is refused again at runtime.

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

[Unreleased]: https://github.com/klarlabs-studio/kiln/compare/v0.1.3...HEAD
[0.1.3]: https://github.com/klarlabs-studio/kiln/releases/tag/v0.1.3
[0.1.2]: https://github.com/klarlabs-studio/kiln/releases/tag/v0.1.2
[0.1.1]: https://github.com/klarlabs-studio/kiln/releases/tag/v0.1.1
[0.1.0]: https://github.com/klarlabs-studio/kiln/releases/tag/v0.1.0
