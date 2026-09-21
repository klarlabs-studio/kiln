# Kiln audit

**Subject:** [klarlabs-studio/kiln](https://github.com/klarlabs-studio/kiln) at `main`  
**Version in tree:** 0.6.0 (module `go.klarlabs.de/kiln`, Go 1.25, toolchain `go1.25.14`)  
**Scope:** product understanding, architecture, trust model, security, correctness, documentation drift, test and release posture  
**Method:** read the public docs and the implementation they describe, then a second pass over architecture and security-sensitive surfaces. No exploit, payload, or reproduction procedure is included.  
**Date:** 2026-09-21

---

## What kiln is

Kiln is a **signed-artifact factory** for a self-hosted build box. It is deliberately not a GitHub Actions clone, not a `runs-on` worker, and not a CD product.

It sits in a three-piece Klarlabs chain:

```
developer
  → warden   (local gate + signed note on the commit)
      → kiln     (re-prove if needed, build, cosign, publish, Checks)
          → registry (image@sha256 + moving tag)
              → rollopsd (imagePolicy / plan / apply / verify / rollback)
```

| Piece | Job |
|---|---|
| **Warden** | Source gate. `.warden.yaml` is the only check language. Signed note on `refs/notes/warden`. |
| **Kiln** | Remote re-prove (unless a trusted note lets it skip), `docker` build/push, `cosign` sign, GitHub Checks, optional automation (`tasks`, `services`). |
| **Nox** | Optional scanner kiln may invoke. Not CI. |
| **RollOps** | CD. `imagePolicy`, plan, apply, drift-verify, rollback. Kiln never applies. |

The claim kiln competes on is narrow and stated honestly in `docs/competitive.md`: **the artifact leaves with two independently signed statements, and neither of them is kiln vouching for somebody else's work.**

1. **Warden's note** on the commit — the configured checks ran and passed (ed25519, carried unmodified).
2. **Kiln's SLSA v1 provenance** on the artifact — it was built from that commit, and whether this build ran the checks or inherited a trusted note.

GitHub stays the forge (PRs, Checks, usually GHCR). Kiln takes the compute and the build provenance. Status: OSS MVP, MIT, single-tenant, self-hosted.

The operator it can serve honestly is also narrow: private source outside GitHub Enterprise Cloud (where Artifact Attestations are not free), an existing Linux box, and a source gate whose verdict they want *carried* rather than paraphrased. For a public repo already on Actions, GitHub's own attestations are strictly less to operate.

---

## What kiln does

### Surfaces

Same engine, four doors. Every path reduces to `engine.Request` and `Engine.Execute` (or `RunScheduled` for cron-like errands). Isolation is applied *after* the caller states intent, so a new surface cannot talk its way around the rules.

| Surface | Role |
|---|---|
| **CLI** (`kiln`) | Primary. `doctor`, `run`, `watch` / `poll`, `status`, `verify`, `attest`, `prune`, `init`, `login`, `box`, `mcp serve`. |
| **MCP** | Agents: `kiln_doctor`, `kiln_status`, `kiln_run`. Push/tag refused unless `KILN_MCP_ALLOW_RUN=1`. No deploy tool. |
| **HTTP** (`kilnd`) | Optional. Bearer on `/v1/run` and ledger reads; HMAC-SHA256 on `/v1/github/webhook`. Refuses to boot without `KILN_TOKEN`. |
| **Box** | `kiln box install` writes a launchd agent or systemd user timer that ticks `watch --once`. |

### One run

```
queued → isolating → proving → publishing → tasks → succeeded | failed
```

Prove, publish and tasks each check the commit out into a disposable detached worktree. A dirty operator checkout cannot leak into a signed artifact.

**Prove** shells out to `warden run pre-push --attest-only`. A missing `warden` is a prove *failure*, never a skip. A provenance skip requires both isolation permission *and* `warden verify --require-signed --key $KILN_TRUSTED_KEYS`. No pinned keys means no skip.

**Publish** is a list: `kind: image` (docker + cosign on the digest) and `kind: binaries` (goreleaser; a config with no `signs:` block is refused). Every image must produce an immutable `sha-` tag plus at least one moving tag (`latest` or `semver`), because RollOps discovers builds by watching a tag that moves. Cosign signs the digest, never a tag. Nothing leaves unsigned.

**Tasks** are automation that is neither a check nor an artifact: scan upload, remediation PR, docs refresh. A task cannot mint provenance. **Services** are sidecar containers (the Actions `services:` analogue) started before the gate and torn down after the tasks.

**Verify** walks signature → provenance → builder identity → source gate, reporting each link separately. `kiln verify --policy` works against artifacts kiln did not build.

### Isolation matrix

Enforced in `internal/domain/isolation` as a pure function of `(event, fork)`:

| Event | Fork | Secrets | Publish | Provenance skip |
|---|---|---|---|---|
| `pull_request` | yes | no | no | no |
| `pull_request` | no | no | no | yes |
| `push` / `tag` | — | yes | yes | yes |

Unknown event, missing token, failed API call, deleted fork, and `--event pull_request` without a PR number all resolve to **fork**. `--fork` is a floor, never a ceiling.

### Unattended behaviour

- Exclusive `flock` per repository (`.kiln/lock`). `run` refuses (exit 75), `watch` skips, `kilnd` waits.
- `KILN_PHASE_TIMEOUT` (default 45m) bounds each phase separately.
- Failed refs back off 15m → 30m → 1h so a red PR cannot spin the box.
- A new box baselines existing tags and does not republish history.
- Worktrees older than a day are reaped; local `sha-` images and build cache are pruned. The registry is never touched.
- Ledger is `.kiln/state.json`, capped at 500 runs. Git is desired state; losing the ledger costs a duplicate build, not correctness.

### What it will not do

No apply, canary, drift or rollback (`deploy:` / `apply:` are load errors that name RollOps). No Actions runner protocol. No second check language. No matrix, macOS/Windows workers, named workspaces, billing, or hosted compute. Those belong in Studio or in RollOps.

---

## Architecture

Layered DDD, enforced by `internal/arch` (a layer may import inward, never outward):

```
cmd/kiln, cmd/kilnd
  → interfaces/   CLI, MCP, daemon
      → boot/     composition root — the only place that knows every layer
          → application/   engine, watch, poll, ports
              → domain/    config, isolation, run, policy, forge
          → infrastructure/  execx, prove, publish, attest, verify, github, store, …
```

`domain` imports nothing internal. Tests are exempt from the import rule so they can wire real adapters.

Production Go files: 84. Test files: 61. Test functions: 664. That is a high test-to-code ratio for a supply-chain tool, and the comments show why: the recurring failure mode has been “unit tests passing while the real integration was broken.” CI therefore also runs `scripts/e2e-verify.sh` against a real `cosign` and a local registry.

---

## Findings

Severity is impact × likelihood in this codebase, not a CVSS score. “High” means a real integrity or privilege problem an operator can hit without exotic setup. Design tradeoffs that are documented and consistent are called out as such, not scored as bugs.

### High

#### H1. A task can force-push the default branch

`tasks.*.pull_request.branch` is the destination of `git push --force origin HEAD:refs/heads/<branch>` (`internal/infrastructure/task/propose.go`). Validation refuses an empty name, a `refs/` prefix, and `branch == base` when both are set. It does **not** reserve `main` / `master` / the watched ref, and `base` may be empty (repository default).

```yaml
tasks:
  remediate:
    on: [push]
    run: echo pwned > README.md
    pull_request:
      branch: main
      title: "chore: apply remediations"
```

That configuration loads. On the next trusted push (or scheduled fire — see M3) kiln force-updates `main` from the task worktree. The comment in `propose.go` is explicit that `--force-with-lease` is intentionally not used.

A collaborator who can merge a `.kiln.yaml` change therefore has a path to rewrite the branch RollOps and humans treat as desired state, without going through review of that rewrite.

**Fix direction:** refuse the watched ref, common default-branch names, and any branch not under a reserved prefix (`kiln/` is already the documented convention). Treat an empty `base` as the watched ref for the equality check.

#### H2. The pipeline file is loaded from the box checkout, not the commit

Docs (`docs/isolation.md`, README) describe a fork head whose `.kiln.yaml` lists `publish` as being *overruled*. The engine would overrule it — but the file is never read from that head.

`boot.Build` loads `<checkout>/.kiln.yaml` once. `watch` fetches remotes and builds each SHA in a detached worktree, then passes the already-loaded `w.Pipeline` through. `git fetch` does not update the working copy. `watch --every` freezes that snapshot for the life of the process.

Consequences:

- Isolation language about overruling a hostile pipeline is true but vacuous; the hostile file is ignored.
- A commit that *changes* `.kiln.yaml` is built with yesterday's pipeline until someone pulls the box checkout (or restarts a long-lived watcher).
- A dirty or hand-edited `.kiln.yaml` on the box applies to every SHA the box builds, including historical tags.

This is a safer default against H1/M3 (a fork cannot add `services:` or a force-pushing task) and a correctness surprise against the documented model (“the pipeline lives in the repository”). It should be an explicit, tested rule: *operator checkout owns routing; the commit owns checks* — or the pipeline should be read from the worktree of the SHA, under the isolation policy.

#### H3. `POST /v1/run` lets the caller pick the trust event

`handleRun` takes `event` and `fork` from the JSON body and passes them to the engine unchanged (`internal/interfaces/daemon/daemon.go`). There is no check that the SHA is on the watched branch, a tag, or a non-fork head.

A holder of `KILN_TOKEN` can therefore ask for `{ "sha": "<any object in the clone>", "event": "push" }` and receive `isolation.For(push, false)`: secrets, publish, provenance skip. Fork PR heads that `watch` has already parked under `refs/kiln/pr/` are in that clone.

The other surfaces do not leave this open:

| Surface | Who decides event / fork |
|---|---|
| Webhook | HMAC-authenticated GitHub payload (`github.ParseDelivery`) |
| MCP | Push/tag refused unless `KILN_MCP_ALLOW_RUN=1`; missing PR number → `ForkUnknown` |
| CLI `kiln run` | Same capability, but it is a local operator command, not a network API |
| `kilnd` JSON | Caller-supplied; `fork` defaults to `false` |

`KILN_TOKEN` is already a signing-and-publish credential. H3 is how a leaked token becomes “sign any commit this box can see,” not only “rebuild what Git just pushed.” MCP extra-gates the same request; kilnd does not.

**Fix direction:** treat `KILN_TOKEN` as tier-0 in the docs. For `event=pull_request`, require `pr` and resolve fork via the API (see M7). For `push`/`tag`, require the SHA to be reachable from the configured remote refs before granting publish policy — or refuse JSON-triggered publish entirely and leave that to webhooks and the CLI.

### Medium

#### M1. The source verdict is attached best-effort

The product differentiator is the *carried* warden envelope. `Engine.sourceSummary` treats a missing or failed `SourceAttestation` as a warning and publishes build provenance alone (`internal/application/engine/engine.go`). Adoption is easier; the chain a verifier is told to expect is not a property of a successful publish.

A consumer that does not run `kiln verify --policy` with `source.required: true` (and RollOps with gates configured — off by default) will accept an image that kiln signed but never bound to a source note.

**Fix direction:** make source-summary attachment a pipeline or environment policy (`required` / `best-effort`), defaulting to required once a box has pinned trusted keys. Keep best-effort as the adoption ramp, not the implicit default on a publishing box.

#### M2. `Run.Clone` does not copy `Tasks`

`Clone` deep-copies `Tags` and `Artifacts` so the ledger cannot be mutated through a returned pointer. `Tasks` is left sharing the backing array. `TestCloneIsDeep` and `TestStoreHandsOutClones` only exercise `Tags`.

The engine appends to `r.Tasks` during the task phase and persists between tasks. A caller that `Get`s a run and mutates `Tasks[i]` aliases the in-memory ledger. File store re-reads from disk, so the durable file is safer than `store.Memory` (used by tests and, if wired, MCP). This is a correctness bug in a type whose comment says the opposite.

#### M3. Scheduled tasks run with secrets and may propose

`RunScheduled` builds `isolation.Policy{Secrets: true, Skip: true}` and never publishes. That is the right *publish* answer — a schedule is not evidence anything changed. It is the wrong *write* answer when combined with H1: a `schedule` task can hold `GITHUB_TOKEN` and force-push.

Mark-as-fired-before-run is correct (avoids a crash loop that opens PRs). The trust boundary is not.

#### M4. Secret scrubbing is a denylist

`execx.Scrub` drops names containing `TOKEN`, `SECRET`, `PASSWORD`, `PASSWD`, `CREDENTIAL`, `APIKEY`, `API_KEY`, `PRIVATE_KEY`, `ACCESS_KEY`, `SESSION_KEY`, `AUTH`, plus an explicit set (`GITHUB_TOKEN`, `SSH_AUTH_SOCK`, `DOCKER_CONFIG`, cloud keys, …). This is documented as a deliberate trade: an allowlist would break unknown-but-benign variables.

What slips through is also documented in spirit and worth naming: `DATABASE_URL`, `DSN`, `CONNECTION_STRING`, `JDBC_URL`, `REDIS_URL`, `PGURL`, `MONGO_URI`, and any `*_PEM` / `*_KID` that does not match the markers. Fork prove/tasks inherit that environment.

Not a bypass of isolation — isolation still withholds the kiln-named credentials — but a fork gate that can read a URL-shaped secret on the box can exfiltrate it. Adding the common URL/DSN names to `secretNames` is cheap; an operator escape hatch (`KILN_KEEP_ENV`) is better than telling people to rename secrets.

#### M5. No sandbox around repository-authored commands

Stated in `SECURITY.md` and true: the worktree isolates from the operator's dirty checkout, not from the user kiln runs as. `warden`, the Dockerfile, goreleaser, and `tasks[].run` (`sh -euc`) all execute with that user's docker socket, git remotes, and home directory.

This is an accepted product constraint (the box *is* the toolchain). It means `.warden.yaml`, `Dockerfile`, `.goreleaser.yaml`, and `tasks` on a trusted event are equivalent to shell on the build user. Review of those files is the control; kiln cannot be the control.

#### M6. Service containers are unhardened

`docker run --detach --rm --name … --publish 127.0.0.1::<port> <image>`. No digest pin, no `--read-only`, no `--cap-drop=ALL`, no user, no network namespace beyond default bridge + loopback publish. `ready` is `docker exec … sh -c <repo string>`.

Today H2 keeps the image name operator-authored. If the pipeline is ever read from the commit (the documented model), a pull request can start an arbitrary image on the box before the gate runs. Even under today's load path, a merged `services:` change is “run this image as the docker daemon.” Doctor already warns when `ready` is missing; it should also warn on an unpinned tag.

#### M7. kilnd defaults an unknown pull request to same-repo

`docs/isolation.md` and the CLI/MCP paths agree: if kiln cannot tell whether a PR head is a fork, it is a fork. `cli/run.go` `resolveFork` and `cli/mcp.go` `facade.Run` both return `boot.ForkUnknown` when `--pr` / `pr` is absent.

`daemon.execute` only calls `ResolvePullFork` when `event == pull_request && pr > 0`. A body `{ "event": "pull_request", "sha": "…" }` therefore keeps `fork=false` (the JSON zero value). That grants provenance skip. Secrets and publish stay off, so this is narrower than H3, but it is a real inconsistency with the documented fail-closed rule: a trusted warden note on that commit can stand in for a re-prove.

#### M8. Build-secret ids are promised in provenance and then dropped

`docs/configuration.md` and `ports.AttestInput.SecretIDs` say the secret *ids* (never values) are recorded so an incident can answer “what did this token build?” `publish.Docker` sets `in.SecretIDs = req.Artifact.SecretIDs()`. `attest.Build` copies `BuildArgs` into `externalParameters` and never writes `SecretIDs` onto the predicate (`internal/infrastructure/attest/attest.go`). The field is dead on the way to the artifact.

This is a contract gap, not a leak. The ids appear in the operator-facing plan string; they do not travel with the digest.

#### M9. MCP `kiln_run` does not take the repository lock

CLI `kiln run` refuses when `.kiln/lock` is held (exit 75). `kiln watch` skips. `kilnd` waits. `facade.Run` in `internal/interfaces/cli/mcp.go` calls `Engine.Execute` with no `TryAcquire`. An agent and a cron tick can share a checkout: overlapping worktrees, interleaved ledger writes, two publishes of the same SHA.

`--dry-run` / `doctor` / `status` correctly never lock. `kiln_run` is a write and should follow `kiln run`.

### Low

#### L1. `SECURITY.md` still hardcodes the v0.1.0 certificate identity

README was corrected after that instruction made later releases look forged. The security policy's “Verifying a release” block still names `...@refs/tags/v0.1.0`. Anyone following the security policy against a current release gets a false “tampered” result — the exact trust failure the README now warns about.

#### L2. Bearer comparison leaks token length

`subtle.ConstantTimeCompare` on the raw bearer vs `KILN_TOKEN` is not constant-time when lengths differ. Webhook HMAC comparison uses `hmac.Equal` after a scheme check and is fine. Bind kilnd to loopback by default (already `127.0.0.1:8088`); if `KILN_ADDR` is opened to a network, hash-and-compare both sides.

#### L3. Authenticated `POST /v1/run` has no rate limit

A stolen `KILN_TOKEN` is already game-over (it can publish). Without a limit, the same token is also a cheap way to pin the box: each call is a synchronous build. Webhooks are 202 + 60m background timeout and wait on the repo lock, which is better.

#### L4. `keep` globs are `filepath.Glob`

`**` is not recursive. `keep: ["**/*.sarif"]` matches nothing and is *reported* (good), but the configuration docs show `*.sarif` and imply general globs. Either document the subset or walk with `fs.WalkDir`.

#### L5. CI coverage is off

`.github/workflows/ci.yml` passes `coverage: false` into the shared go-ci bar, with “No .coverctl.yaml threshold yet.” 664 tests without a floor will shrink quietly. The e2e verify job and the release-workflow architecture test are the stronger checks and should stay.

#### L6. Image build stage is not digest-pinned

Actions are pinned to commits. `Dockerfile` uses `golang:1.25-bookworm` and `gcr.io/distroless/static-debian12:nonroot` by tag. The runtime image is also not a builder: it has no git/docker/warden/cosign. The file says so; an operator who still treats it as a worker image will discover that at prove time.

#### L7. Repository lock is Unix-only

`internal/infrastructure/lock/flock_other.go` fails closed on non-Unix. Correct. The README brew cask claims macOS and Linux; Windows is out of scope and should stay that way in the install docs.

#### L8. Makefile `examples-check` does not distinguish policy files

CI runs `kiln doctor --policy` on `*policy*.yaml` and `--config-only --pipeline` on pipelines. `make examples-check` runs `--config-only --pipeline` on every `examples/*.yaml`. A policy file passed as a pipeline is a load error. `make release-check` depends on this target.

#### L9. `kiln status` does not list kept task files

`docs/configuration.md` says matches are copied to `.kiln/runs/<run-id>/<task>/` and that `kiln status` lists them. The files are written. `printStatus` in `internal/interfaces/cli/status.go` prints run metadata, digest, and tags. It does not print `r.Tasks` or walk the keep directory. An operator following the docs looks in the wrong place.

#### L10. `internal/infrastructure/gitcli` has no tests

Watch discovery, tag peeling, and PR-ref listing all go through this adapter. Every other critical adapter (`publish`, `attest`, `github`, `task`, `watch`) has dedicated tests; `gitcli` relies on higher-level watch tests and a fake `execx`. A format-string change in `for-each-ref` would show up as “PRs vanished,” not as a red unit test.

### Documentation drift (not defects in the binary)

These are audit findings because a supply-chain tool's docs *are* part of the trust story.

| Document | Drift |
|---|---|
| `docs/backlog.md` | Lists scheduled tasks, `keep`, task PRs, and `services:` as unimplemented. All four are in `config`, `engine`, `watch`, `task`, and `service` and described as live in `docs/configuration.md`. The remaining real item is declarative SARIF upload. |
| `docs/rollops-handoff.md` | “An in-toto/SLSA export can be layered on later.” It has shipped. The rest of the handoff (tag contract, check names, no deploy) is still accurate. |
| `CONTRIBUTING.md` | Architecture section still names `internal/engine`, `internal/cli`, `internal/prove`. The tree is `application/`, `domain/`, `infrastructure/`, `interfaces/`, `boot/`. |
| `SECURITY.md` | Stale verify-blob tag (L1). The model section is otherwise aligned with the code, including the “worktree is not a sandbox” sentence. |
| `docs/competitive.md` | Last verified 2026-08-18. The GitHub private-repo attestation restriction is the load-bearing fact; it is due a refresh. |
| `docs/configuration.md` | Says secret ids land in provenance (M8) and that `kiln status` lists kept files (L9). Neither is true in the binary. |
| `docs/isolation.md` | “`kiln run --event pull_request` with no `--pr` → fork.” True for CLI and MCP; false for kilnd (M7). |

---

## Strengths

Kiln is unusually explicit about what it will not do, and the code generally matches that posture.

**Trust boundaries are small and tested.** `isolation.For` is a pure function with an exhaustive matrix. Fork detection fails closed in the CLI, the watcher, and the webhook parser. `--fork` cannot be turned off by a later API result.

**Surfaces cannot override the isolation *function*.** They can still choose the inputs. CLI, MCP, HTTP and webhook all call the same engine, so `publish` on a `pull_request` event is still suppressed. H3 is the remaining hole: kilnd lets the caller name the event. MCP push/tag is extra-gated. Webhook rejects empty secret and SHA-1 with the same 401 as a bad MAC. `deploy:` is a load error, not a feature request.

**Signing failures are loud.** Missing `cosign` fails publish. Goreleaser without `signs:` is refused before the build. `KILN_COSIGN_KEY` holding PEM (or its base64) is rejected in `boot` before the logger exists; `Cmd.String` redacts leftovers. The ledger stores `ExitError.Summary()`, not subprocess stderr — a response to a real key leak into `.kiln/state.json` (0.6.0).

**Untrusted input is treated as hostile.** `keep` and `materialize` refuse `..` and absolute paths, and resolve symlinks before copy. Unknown YAML keys are load errors. JSON API bodies `DisallowUnknownFields`. Build `args` have no env passthrough; build `secrets` are `env://` only and checked present before docker runs. The ids are supposed to be recorded on the predicate (M8) and today are not.

**Operations look like they have been on a real box.** Tag baseline, closed-PR filtering (`refs/pull/N/head` is immortal), failure backoff (205 failed runs in an afternoon is cited from production), PATH pinning in `box install`, keychain ACL so a launchd tick does not hang on a dialog, docker prune that never deletes a moving tag or a foreign repository.

**Release engineering is careful.** Shared CI workflow pinned to a commit. Release job overrides `id-token` off for the test job so the suite sees CI's environment (an architecture test fails the build if that regresses). Keyless cosign over `checksums.txt` names the workflow and the tag. CycloneDX SBOM per archive.

**Layering is mechanically enforced.** `internal/arch` walks every non-test file. Domain stays free of I/O.

---

## Test and CI posture

| Check | State |
|---|---|
| Unit / integration tests | Broad. Isolation, webhook HMAC, config KnownFields, publish plan, goreleaser `signs:`, keep traversal, materialize, schedule, backoff, baseline, ledger redaction, cosign key refusal, verify policy, daemon auth all have dedicated tests. |
| Race | `make race`; release workflow runs `go test -race`. |
| Real cosign | `verify-e2e` job, not opt-in. Already caught a `condense()` bug that rendered refusals as `0 < 1`. |
| Architecture | Import rule + release-permissions test. |
| Examples | CI validates both pipeline and policy examples; Makefile does not (L8). |
| Coverage floor | Disabled (L5). |
| Lint | golangci-lint v2 org bar; gosec deliberately omitted in favour of nox taint analysis in the shared workflow. |

Gaps relative to the findings: no test that `pull_request.branch: main` is refused; no test that `Clone` isolates `Tasks`; no test that a long-lived watcher reloads `.kiln.yaml` (because it does not); no test that a successful publish without a source VSA is a policy choice rather than a warn-and-continue; no test that kilnd without `pr` is a fork; no test that `SecretIDs` appear on the predicate; no lock around MCP `kiln_run`; no `gitcli` unit tests.

---

## Residual risk (accepted, not scored)

These are product decisions. An auditor should not “fix” them without changing what kiln is.

- **Single box, one lock, serial work.** Feedback is slower than Actions. A five-minute schedule over a twenty-minute build is safe and slow.
- **Signing identity is ambient.** Kiln does not manage keys. A self-hosted box without `KILN_COSIGN_KEY` hangs in the device-flow; `doctor` warns.
- **RollOps verification is off until configured.** An unsigned image deploys on an unconfigured daemon. The README now calls this a default, not a missing capability. Anyone measuring “does the chain hold” must configure the consumer.
- **Kiln's own releases are goreleaser + keyless, not `kiln publish`.** Verifying them with `kiln verify` against `provenance.intoto.jsonl` fails on a missing file. The README is correct; the two release shapes must not be conflated.
- **OSS is not multi-tenant.** `kilnd` is one directory, one pipeline, one token.
- **Ecosystem is two artifact kinds.** Anything that is not `docker build` or goreleaser is out of scope.

---

## Recommended order of work

1. **Refuse dangerous task branches** (H1) and treat empty `base` as the watched ref. Add a regression test that `branch: main` is a load error.
2. **Harden kilnd `POST /v1/run`** (H3, M7): unknown PR → fork; do not grant push/tag policy to a SHA that is not on a configured ref. Document `KILN_TOKEN` as equivalent to registry write plus signing.
3. **Decide and document pipeline authorship** (H2). Either “the box checkout is the pipeline, the commit is the gate” — and reload it each `watch --once` / each `--every` tick after an optional fast-forward of the tracked branch — or load from the worktree and run `services` / `tasks` through the same isolation as secrets.
4. **Lock MCP `kiln_run`** the way `kiln run` does (M9). Copy `Tasks` in `Run.Clone` (M2).
5. **Serialize `SecretIDs` onto the SLSA predicate** (M8), or stop promising them.
6. **Make source-summary attachment configurable and default-strict when trusted keys exist** (M1).
7. **Refresh stale docs** (`backlog.md`, `CONTRIBUTING.md`, `SECURITY.md` verify command, `rollops-handoff.md` SLSA paragraph, `configuration.md` keep/status and secret-id claims) so the audit trail matches 0.6.0.
8. **Widen the secret-name list** with URL/DSN forms (M4); pin or warn on `services[].image` tags (M6); show kept files in `kiln status` (L9).

Nothing in this list requires growing kiln toward CD, a second check language, or an Actions runner. Those remain category errors.

### Follow-up (this branch)

Items 1–8 above have been implemented. The findings remain as the audit
recorded them; the table is where the work landed.

| Item | Where it landed |
|---|---|
| 1 H1 `kiln/*` writes | `internal/domain/write`; config load error for `branch: main`; empty `base` is the watched ref |
| 2 H3 / M7 kilnd | `internal/application/authority` derives event/fork/membership; JSON cannot manufacture push/tag; unknown PR is a fork; `KILN_TOKEN` documented as a publish credential |
| 3 H2 pipeline authorship | Operator checkout owns `.kiln.yaml`; identity recorded in provenance; `watch --every` reloads each tick |
| 4 M9 / M2 | MCP `kiln_run` takes the repo lock; `Run.Clone` copies `Tasks` |
| 5 M8 SecretIDs | SLSA `externalParameters` records ids, never values |
| 6 M1 evidence | `evidence.source: required \| best-effort`; required is the default when `KILN_TRUSTED_KEYS` is set |
| 7 docs | `intent.md`, `CONTRIBUTING.md`, `SECURITY.md`, `isolation.md`, `configuration.md`, `backlog.md` |
| 8 M4 / M6 / L9 | URL/DSN/`*_PEM` scrubbing; unpinned service-image warning; `kiln status` lists kept files |

Further on this branch: the engine consumes `trust.Context` rather than
loose caller fields; coverage floors live in `.coverctl.yaml`;
`filepath.Glob` (no `**`) is documented for `keep`; an architecture
test refuses a surface that constructs `engine.Request` or calls
`Engine.Execute`.

Kiln's own Dockerfile bases are now digest-pinned (L6). Still later:
an offline evidence bundle, a process sandbox, and HTTP rate limits.
Those do not change the handoff.

---

## Summary judgement

Kiln is a small, opinionated build-and-attest tool with a clear place in a larger system and an unusually adult threat model for an 0.x project. The isolation matrix, fail-closed fork handling, unsigned-publish refusal, worktree discipline, and the recent key-material / ledger-redaction work are real engineering, not brochure security.

The main gaps are where the *new* surface (`tasks`, especially `pull_request` + `schedule`) meets write credentials, where kilnd lets the caller name the trust event, and where the docs still describe a world the code has already left (backlog items that shipped) or a world the code never implemented (pipeline loaded from the commit; secret ids on the predicate). Fix those without widening the product and the claim — two authorities, one commit, nothing unsigned — stays honest.
