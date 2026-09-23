The items below were the original migration list. Schedule, `keep`,
task `pull_request`, `services:`, and the Gitea/Forgejo forge door have
landed. Near-term work now lives in [intent.md](intent.md): strengthen
evidence and authority rather than grow the workflow language. The
remaining named backlog item is declarative SARIF upload — a product
decision, because it increases GitHub coupling.

## Fire scheduled tasks from the watch loop

**Landed.** Watch fires due `on: [schedule]` tasks against the tracked
ref's head. The ledger remembers the last run per task so an interval
survives a restart. A schedule is not push/tag authority: secrets are
granted only to the task that proposes a write, not to every task due
in the same tick.

---

## Upload a task's SARIF to code scanning

Seven repos run github/codeql-action/upload-sarif so nox findings reach the Security tab. Give a task an `upload: {sarif: path}` declaration: kiln reads the file the task produced, gzips and base64s it, and posts it to the code-scanning API for the commit under test. Declarative rather than leaving it to `gh api` in the command, because the payload shape (commit_sha, ref, checkout_uri, base64 gzip) is fiddly enough that everyone gets it wrong once, and because a task that silently failed to upload looks identical to a clean scan. Fails the task if the upload is rejected. Note: code scanning on private repositories requires GitHub Advanced Security — verify the plan before relying on this for the private-repo migration.

---

## Open a pull request from a task

**Landed.** A task may declare `pull_request: {branch, title, body, labels}`.
The branch must live under `kiln/`; `branch: main` is a load error. Empty
`base` is the watched ref. Refused on an untrusted head. Does nothing when
the worktree is clean.

Idempotent by branch name, so a daily remediation run updates its existing
PR rather than opening thirty.

---

## Retain a task's output files

**Landed.** `keep:` copies matches into `.kiln/runs/<run-id>/<task>/`
before the worktree is destroyed. `kiln status` lists them. Patterns are
`filepath.Glob` (no recursive `**`). Retention is bounded.

Deliberately local files rather than an upload to GitHub. A task that
wants them elsewhere can rsync them.

---

## Migrate one private repository off Actions end to end

The proof, and the thing that will find what the feature list missed. Pick one private repo, express its whole gate in `.warden.yaml` (today kiln's own has vet/test/lint while Actions additionally runs the coverage gate, nox, build, examples, goreleaser check and govulncheck), point a kiln box at it with `kiln watch --every 1m`, require the Kiln checks in branch protection, and delete the workflow file. Write down what broke. Explicit non-goals: GitHub Pages deploys stay in Actions (a deployment belongs with RollOps), and npm publishing of public packages stays in Actions because npm provenance can only be minted from GitHub Actions or GitLab CI on cloud runners — though npm issues no provenance for private repositories at all, so private packages lose nothing by moving.

---

## Service containers for the gate and tasks

**Landed.** `services:` starts sidecar containers before the gate and
tears them down after the tasks. Host ports are allocated dynamically.
An image without `@sha256:<64-hex>` is a load error. Containers run with
`--cap-drop ALL`, `no-new-privileges`, `--init`, `--pids-limit 256`, and `--tmpfs /tmp`.

Host ports are exported as `KILN_SERVICE_<NAME>_HOST` / `_PORT`.
Readiness is waited for with a timeout.

---
