# Kiln

**Warden proves a commit. Kiln turns that commit into a signed container image. RollOps is the only thing allowed to ship it.**

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
go install go.klarlabs.de/kiln/cmd/kiln@latest
go install go.klarlabs.de/kiln/cmd/kilnd@latest   # optional HTTP surface
```

Kiln shells out to tools that must already be on the box:

| Tool | Needed for |
|---|---|
| `git` | always |
| `warden` | always — a missing gate is a **prove failure**, never a skip |
| `docker` | publishing |
| `cosign` | publishing — RollOps refuses to deploy an unsigned digest |
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
  image: ghcr.io/klarlabs-studio/kiln
  tags: [sha, latest]   # sha + at least one of latest|semver
  sign: cosign
  platforms: [linux/amd64]
  dockerfile: Dockerfile
  context: .
watch:
  remote: origin
  ref: main
  pull_requests: true
  tags: true
```

Unknown fields are rejected. A `deploy:` or `apply:` key is a load error that names RollOps. `on.tag` inherits `on.push` when omitted.

See [`examples/pipeline.example.yaml`](examples/pipeline.example.yaml) for the Glossa-shaped version, and [`docs/configuration.md`](docs/configuration.md) for the full schema.

### Environment

| Variable | Role |
|---|---|
| `KILN_DB` | Run ledger path (default `.kiln/state.json`) |
| `KILN_DRY=1` | Plan tags; call neither docker nor cosign |
| `KILN_WARDEN` / `KILN_NOX` | Binary names |
| `KILN_TRUSTED_KEYS` | Comma-separated signer keys that permit a provenance skip. **Operator environment, never the PR head.** |
| `GITHUB_TOKEN` / `GH_TOKEN` | Checks and pull request fork lookup |
| `KILN_MCP_ALLOW_RUN=1` | Permit push/tag runs on the MCP surface |
| `KILN_ADDR` | kilnd bind address (default `127.0.0.1:8088`) |
| `KILN_TOKEN` | kilnd bearer token — **required to boot** |
| `KILN_WEBHOOK_SECRET` | GitHub webhook HMAC |
| `KILN_DIR` | Repository directory for kilnd |
| `GITHUB_REPOSITORY` | `owner/name`, when the git remote is absent |
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

## One run

```
queued → isolating → proving → publishing → succeeded | failed
```

Prove and publish each check the commit out into their own disposable worktree. A dirty operator checkout can never leak into a signed artifact.

The provenance skip requires **both** that policy allows it and that `warden verify --require-signed --key $KILN_TRUSTED_KEYS` passes. No pinned keys means no skip: an unpinned verify would accept a note signed by a key the pull request author generated five minutes ago.

---

## Publish contract

Every successful publish produces an **immutable sha tag plus at least one moving tag**.

| Tag | Example | RollOps |
|---|---|---|
| sha | `ghcr.io/felixgeelhaar/glossa-api:sha-abc1234` | pin |
| latest | `…:latest` | `imagePolicy.mode: digest` |
| semver | `…:v0.2.0` | `imagePolicy.mode: minor` |

A sha-only tag list is a config error: `imagePolicy` cannot follow a moving target that never moves.

Cosign signs the **digest**, not a tag. Check names are a contract: **`Kiln / Prove`** and **`Kiln / Publish`**. Branch protection and RollOps' PR writeback wait on them, so renaming one needs a migration note.

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
| `kiln poll` | Branch-only subset of watch; needs no token at all |
| `kiln status [run-id]` | Read the ledger |
| `kiln mcp serve` | Stdio MCP |

Exit codes: `0` ok, `2` the gate rejected the change, `3` configuration or toolchain, `64` usage. Cron can tell "your code is wrong" from "this machine is broken".

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

---

## Boundaries

Kiln has no apply, canary, drift or rollback, and will not grow them. It does not implement an Actions runner protocol. It does not read a second check language. OSS is single-tenant and self-hosted; named workspaces, billing and hosted workers belong in Studio.

Out of this MVP: UI/dashboard, named workspaces, billing, hosted workers, macOS runners, SQLite, in-toto/SLSA export, gRPC.

## License

MIT
