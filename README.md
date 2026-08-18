<p align="center">
  <img src="assets/logo.svg" alt="kiln" width="116" height="116">
</p>

<h1 align="center">kiln</h1>

<p align="center">
  <a href="https://github.com/klarlabs-studio/kiln/actions/workflows/ci.yml"><img src="https://github.com/klarlabs-studio/kiln/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/klarlabs-studio/kiln/releases/latest"><img src="https://img.shields.io/github/v/release/klarlabs-studio/kiln?sort=semver" alt="Release"></a>
  <a href="https://pkg.go.dev/go.klarlabs.de/kiln"><img src="https://pkg.go.dev/badge/go.klarlabs.de/kiln.svg" alt="Go Reference"></a>
  <a href="https://goreportcard.com/report/go.klarlabs.de/kiln"><img src="https://goreportcard.com/badge/go.klarlabs.de/kiln" alt="Go Report Card"></a>
  <a href="https://slsa.dev/spec/v1.0/levels"><img src="https://img.shields.io/badge/SLSA-v1%20provenance-f59e0b" alt="SLSA v1 provenance"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/klarlabs-studio/kiln" alt="License: MIT"></a>
</p>

**Warden proves a commit. Kiln turns that commit into a signed artifact. RollOps is the only thing allowed to ship it.**

Kiln is a signed-artifact factory. It is deliberately *not* a GitHub Actions clone, not a `runs-on` worker, and not a CD product. GitHub stays the forge — pull requests, Checks, GHCR. Kiln takes the compute and the build provenance.

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
| **Kiln** | Remote re-prove (unless a trusted note lets it skip), `docker` build/push, `cosign` sign, GitHub Checks. |
| **Nox** | Optional scanner Kiln may invoke. Not CI. |
| **RollOps** | CD. `imagePolicy`, plan, apply, verify, rollback. Enforces cosign. Kiln never applies. |

Status: OSS MVP, MIT, module `go.klarlabs.de/kiln`, Go 1.25.

---

## Install

```bash
brew tap klarlabs-studio/tap && brew install --cask kiln   # macOS and Linux
```

```bash
go install go.klarlabs.de/kiln/cmd/kiln@latest
go install go.klarlabs.de/kiln/cmd/kilnd@latest   # optional HTTP surface
```

Or take the archive from [Releases](https://github.com/klarlabs-studio/kiln/releases) —
and verify it, since a tool that argues for verification and ships unverifiable
downloads is arguing against itself. Every release carries a cosign bundle over
the checksum manifest, signed keylessly by the release workflow, plus a
CycloneDX SBOM per archive:

```bash
cosign verify-blob \
  --bundle checksums.txt.bundle \
  --certificate-identity \
    "https://github.com/klarlabs-studio/kiln/.github/workflows/release.yml@refs/tags/v0.1.0" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

The identity is the point: the signature names the workflow and the tag that
produced the file, so it cannot be reused for a build made anywhere else.

Kiln shells out to tools that must already be on the box:

| Tool | Needed for |
|---|---|
| `git` | always |
| `warden` | always — a missing gate is a **prove failure**, never a skip |
| `docker` | publishing an image |
| `goreleaser` | publishing binaries |
| `cosign` | publishing anything — nothing leaves kiln unsigned |
| `nox` | only when `prove.nox: true` |

`kiln doctor` tells you which of these are missing before a build discovers it the hard way.

---

## Quick start

```bash
cd your-repo

# 1. Validate. Runs no gate, builds nothing, pushes nothing.
kiln doctor

# 2. Build one commit.
kiln run --sha HEAD --event push --ref refs/heads/main

# 3. Or let it discover work. This is the cron shape.
kiln watch --once
```

A repository with no `.kiln.yaml` still works: Kiln proves every event and publishes nothing. That is the right behaviour for a library.

---

## Configuration

### `.warden.yaml` — the checks

Owned by Warden. Kiln shells out to `warden run pre-push --attest-only` and does not read this file. It has no opinion about what your checks are.

`--attest-only` is warden's CI mode: run the gate, write the provenance note, move no refs. The bare form is a git hook — it also pushes — and kiln never moves a branch.

### `.kiln.yaml` — publish and routing

```yaml
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove, publish]
prove:
  from: warden          # the only accepted value
  nox: false
publish:
  - kind: image                        # the image RollOps deploys
    image: ghcr.io/klarlabs-studio/kiln
    tags: [sha, latest]                # sha + at least one of latest|semver
    sign: cosign
    platforms: [linux/amd64]
    dockerfile: Dockerfile
    context: .
  - kind: binaries                     # the release a human downloads
    from: goreleaser                   # .goreleaser.yaml owns the mechanics
    on: [tag]
watch:
  remote: origin
  ref: main
  pull_requests: true
  tags: true
```

`publish:` is a **list**, because "signed artifact" is not a synonym for "container image": one proven commit routinely yields an image *and* a set of release binaries. A third kind later is one more list entry rather than one more top-level key.

Unknown fields are rejected. A `deploy:` or `apply:` key is a load error that names RollOps. `on.tag` inherits `on.push` when omitted.

See [`examples/pipeline.example.yaml`](examples/pipeline.example.yaml) for the Glossa-shaped version, and [`docs/configuration.md`](docs/configuration.md) for the full schema.

### Environment

| Variable | Role |
|---|---|
| `KILN_DB` | Run ledger path (default `.kiln/state.json`) |
| `KILN_DRY=1` | Plan tags; call neither docker nor cosign |
| `KILN_WARDEN` / `KILN_NOX` / `KILN_GORELEASER` | Binary names |
| `KILN_TRUSTED_KEYS` | Comma-separated signer keys that permit a provenance skip. **Operator environment, never the PR head.** |
| `GITHUB_TOKEN` / `GH_TOKEN` | Checks and pull request fork lookup |
| `KILN_MCP_ALLOW_RUN=1` | Permit push/tag runs on the MCP surface |
| `KILN_ADDR` | kilnd bind address (default `127.0.0.1:8088`) |
| `KILN_TOKEN` | kilnd bearer token — **required to boot** |
| `KILN_WEBHOOK_SECRET` | GitHub webhook HMAC |
| `KILN_DIR` | Repository directory for kilnd |
| `GITHUB_REPOSITORY` | `owner/name`, when the git remote is absent |
| `KILN_PHASE_TIMEOUT` | Bound on each phase (default `45m`; `0` disables) |
| `KILN_BUILD_CACHE_MAX_AGE` | Prune docker build cache older than this (default `168h`; `0` disables) |
| `KILN_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |

---

## Isolation

Policy is a function of event and fork, enforced in the engine — **not** in the pipeline file. A `.kiln.yaml` edited on a fork pull request to demand a publish gets overruled, not obeyed.

| Event | Fork | Secrets | Publish | Provenance skip |
|---|---|---|---|---|
| `pull_request` | yes | no | no | no |
| `pull_request` | no | no | no | yes |
| `push` / `tag` | — | yes | yes | yes |

Without `GITHUB_TOKEN`, every pull request is treated as a fork. Fork pull requests run the gate with a scrubbed environment: no registry credentials, no token, no agent socket.

A same-repo pull request may skip the re-prove but still may not publish. An image built from an unmerged head is one nobody should be able to ship.

---

## The provenance chain

Two statements, one chain, and both are now verifiable by anyone — not just by
the machine that built the artifact.

1. **Warden's note** on the commit: the configured checks ran and passed.
2. **Kiln's attestation** on the artifact: it was built from that commit — and
   whether this build ran the checks or inherited a trusted note.

The second is a [SLSA v1](https://slsa.dev/provenance/v1) predicate in an
in-toto statement, attached with `cosign attest` to the image digest (and to a
release's `checksums.txt` as `provenance.intoto.jsonl`). It pins the commit as
a `gitCommit` resolved dependency, which is exactly what Warden's note is bound
to — that shared commit is what makes the two statements one chain.

```bash
kiln verify ghcr.io/felixgeelhaar/glossa-api@sha256:… --key cosign.pub --dir .
```

```
  ok       signature    cosign accepted the pinned key
  ok       provenance   built from c3f7aca on refs/tags/v0.2.0
  ok       builder      https://github.com/klarlabs-studio/kiln@v0.1.0
  ok       source gate  warden note on c3f7aca

chain verified end to end
```

Each link is reported separately and a break in one does not hide the others —
"the signature is fine but the source gate is missing" is a far more useful
answer than "invalid". The **builder** link is not ceremony: cosign proves a
trusted key signed *an* attestation, not what it says, so anything reading
kiln's `sourceGate` field must first confirm kiln wrote it.

Exit codes: `0` verified, `2` a link broke. A link that could not be checked —
no local clone for the note, no cosign — is reported as `unknown`, and an
unchecked *signature* is a failure while an unchecked *source gate* is a
caveat: the artifact really is signed either way.

Verifying a binary release works on the downloaded files instead:

```bash
cosign verify-blob-attestation --key cosign.pub \
  --type https://slsa.dev/provenance/v1 \
  --bundle provenance.intoto.jsonl checksums.txt
```

---

## One run

```
queued → isolating → proving → publishing → succeeded | failed
```

Prove and publish each check the commit out into their own disposable worktree. A dirty operator checkout can never leak into a signed artifact.

The provenance skip requires **both** that policy allows it and that `warden verify --require-signed --key $KILN_TRUSTED_KEYS` passes. No pinned keys means no skip: an unpinned verify would accept a note signed by a key the pull request author generated five minutes ago.

A skipped re-prove is recorded in the attestation as `sourceGate.reproved: false`, so the distinction between "these checks ran for this artifact" and "this artifact inherited a verdict" survives all the way to whoever verifies it.

---

## Publish contract

### Images

Every successful image publish produces an **immutable sha tag plus at least one moving tag**.

| Tag | Example | RollOps |
|---|---|---|
| sha | `ghcr.io/felixgeelhaar/glossa-api:sha-abc1234` | pin |
| latest | `…:latest` | `imagePolicy.mode: digest` |
| semver | `…:v0.2.0` | `imagePolicy.mode: minor` |

A sha-only tag list is a config error: `imagePolicy` cannot follow a moving target that never moves.

Cosign signs the **digest**, not a tag.

### Binaries

`kind: binaries` delegates cross-compilation, archives, checksums, the changelog and the GitHub Release to **goreleaser** — kiln invents a second release language no more than it invents a second check language. What kiln adds is the guarantee: it reads `.goreleaser.yaml` before building and **refuses to release a config with no `signs:` block**, so a verifiable release stops depending on whether somebody remembered.

The release's identity is the digest of its `checksums.txt`, which covers every archive. That is what lands in the ledger and the Check.

Both kinds report through one **`Kiln / Publish`** check: one commit, one event, one verdict. Check names are a contract — **`Kiln / Prove`** and **`Kiln / Publish`** — and branch protection and RollOps' PR writeback wait on them, so renaming one needs a migration note.

### Division of labour

| File | Owner | Says |
|---|---|---|
| `.warden.yaml` | Warden | what "passing" means |
| `.goreleaser.yaml` | GoReleaser | how binaries are built and released |
| `.kiln.yaml` | Kiln | what to publish, and which events route where |

Versioning and release governance stay with **relicta**; kiln neither picks a version nor writes the notes.

---

## Surfaces

Same engine, four ways in.

### CLI — primary

| Command | Role |
|---|---|
| `kiln version` | Version, commit, build date |
| `kiln doctor` | Validate YAML, print the tag plan, check the toolchain. Runs nothing. |
| `kiln run --sha S --event E` | One-shot build. `--sha HEAD` resolves via git. |
| `kiln watch --once \| --every D` | Fetch branch, PR heads and tags; skip what already succeeded |
| `kiln watch --repos /srv/*` | The same across a fleet, from one process |
| `kiln poll` | Branch-only subset of watch; needs no token at all |
| `kiln status [run-id]` | Read the ledger |
| `kiln verify <ref>` | Walk a published artifact's whole provenance chain |
| `kiln prune [--dry-run]` | Reclaim local docker disk for this pipeline |
| `kiln mcp serve` | Stdio MCP |

Exit codes: `0` ok, `2` the gate rejected the change, `3` configuration or toolchain, `64` usage, `75` another kiln holds the repository. Cron can tell "your code is wrong" from "this machine is broken".

Ctrl-C cancels through `signal.NotifyContext`, so worktrees are cleaned up rather than leaked. Logs are bolt JSON on **stderr**.

### MCP — agents

| Tool | Access |
|---|---|
| `kiln_doctor` | Read-only |
| `kiln_status` | Read-only |
| `kiln_run` | Pull request prove always; push/tag refused unless `KILN_MCP_ALLOW_RUN=1` |

There is no deploy tool, and adding one would be a category error.

### HTTP (`kilnd`) — optional

Cron plus `kiln watch --once` stays the daemon-less default. `kilnd` is for operators who already run a process.

| Route | Auth |
|---|---|
| `GET /healthz`, `/readyz` | none |
| `POST /v1/run` | Bearer `KILN_TOKEN` |
| `GET /v1/runs/{id}`, `GET /v1/runs` | Bearer |
| `POST /v1/github/webhook` | HMAC-SHA256 `KILN_WEBHOOK_SECRET` |

The webhook answers 202 and builds in the background: GitHub's ten-second delivery window is not the build budget. A missing secret is the same 401 as a forged signature.

### GitHub — the human UI

Kiln posts Checks. There is no Kiln web app in OSS, and there does not need to be — humans have the pull request page, agents have MCP, operators have the CLI.

---

## Unattended

Three things make a box safe to leave alone for months.

**One build at a time per repository.** Overlap is not an edge case under cron:
a five-minute build on a one-minute schedule produces five kilns on one
checkout, all deciding the head is unbuilt because none has finished writing a
success yet. An exclusive `flock` per repository serialises them, and the
kernel drops it if a process dies. What "busy" means depends on who asked —
`kiln run` refuses (exit 75, naming the holder), `kiln watch` skips and exits
0, `kilnd` waits. Read-only commands and `--dry-run` never take it.

**Every phase is bounded.** `KILN_PHASE_TIMEOUT` (default 45m) caps the gate
and each artifact's publish separately, so a docker pull that stops answering
cannot pin a watcher. A timeout is reported distinctly from a failure: one
means fix the code, the other means look at the machine.

**Abandoned worktrees are collected.** A run cleans up after itself, but not
through SIGKILL or an OOM kill. Each tick reaps kiln-prefixed temp directories
older than a day, skipping any a live process still holds a lock on — the
kernel drops that lock when a build dies, so the leavings become collectable
without anything having to notice.

**Docker disk is reclaimed.** Each tick keeps the last `keep:` sha-tagged
builds of the images this pipeline publishes (default 10) and prunes build
cache older than `KILN_BUILD_CACHE_MAX_AGE` (default 168h) — usually the
larger reclaim of the two. It never touches a moving tag, never a repository
kiln does not publish, and **never a registry**: a local image is a re-pull
away, a deleted registry digest is a rollback RollOps can no longer perform.

```cron
*/5 * * * *  cd /srv/glossa && /usr/local/bin/kiln watch --once >> /var/log/kiln.log 2>&1
```

Each tick recomputes the full set of interesting refs and drops the ones a **successful** run already covers, so a missed tick self-heals and a doubled tick is a no-op. A failed run is always retried. One failing job does not stop the others — otherwise anybody who can open a pull request could halt the pipeline.

---

## Documentation

- [`docs/configuration.md`](docs/configuration.md) — the full `.kiln.yaml` schema
- [`docs/isolation.md`](docs/isolation.md) — the trust model, in detail
- [`docs/operating.md`](docs/operating.md) — running it unattended, kilnd, troubleshooting
- [`docs/rollops-handoff.md`](docs/rollops-handoff.md) — what RollOps consumes
- [`docs/competitive.md`](docs/competitive.md) — the OSS CI landscape, and where kiln loses

---

## Boundaries

Kiln has no apply, canary, drift or rollback, and will not grow them. It does not implement an Actions runner protocol. It does not read a second check language. OSS is single-tenant and self-hosted; named workspaces, billing and hosted workers belong in Studio.

Out of this MVP: UI/dashboard, named workspaces, billing, hosted workers, macOS runners, SQLite, gRPC. (in-toto/SLSA export was on this list and has since shipped — see [The provenance chain](#the-provenance-chain).)

## License

MIT
