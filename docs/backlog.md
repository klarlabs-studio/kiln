
## Fire scheduled tasks from the watch loop

`tasks: {on: [schedule], every: 24h}` parses, validates and is listed by `kiln doctor`, but nothing executes it — a scheduled task silently never runs. Wire the watch tick to fire due scheduled tasks against the tracked ref's head, recording the last run per task in the ledger so an interval survives a restart and a box that was off overnight does not fire a day's worth of catch-up runs at once. This is the capability 19 nox-remediate workflow uses depend on, and until it exists no scheduled automation can leave Actions.

---

## Upload a task's SARIF to code scanning

Seven repos run github/codeql-action/upload-sarif so nox findings reach the Security tab. Give a task an `upload: {sarif: path}` declaration: kiln reads the file the task produced, gzips and base64s it, and posts it to the code-scanning API for the commit under test. Declarative rather than leaving it to `gh api` in the command, because the payload shape (commit_sha, ref, checkout_uri, base64 gzip) is fiddly enough that everyone gets it wrong once, and because a task that silently failed to upload looks identical to a clean scan. Fails the task if the upload is rejected. Note: code scanning on private repositories requires GitHub Advanced Security — verify the plan before relying on this for the private-repo migration.

---

## Open a pull request from a task

The single biggest Actions dependency in the org: 19 repos call the shared nox-remediate workflow and two more use peter-evans/create-pull-request. A task that modified the worktree can declare `pull_request: {branch, title, body, labels}`; kiln commits the diff, pushes the branch and opens or updates the PR. Idempotent by branch name, so a daily remediation run updates its existing PR rather than opening thirty. Refuses outright on an untrusted head — a fork PR whose task could open a PR against the base repository would be a write primitive handed to anyone. Does nothing when the worktree is clean, and says so, because "no changes needed" and "the tool is broken" must not look the same.

---

## Retain a task's output files

22 uses of actions/upload-artifact across the org — coverage reports, scan output, build logs kept for after-the-fact reading. A task declares `keep: [globs]`; kiln copies the matches out of the disposable worktree into `<repo>/.kiln/runs/<run-id>/<task>/` before the tree is destroyed, and `kiln status <run-id>` lists what is there. Retention is bounded the way the ledger and the docker prune are, because a build box that keeps every artifact forever fills its disk and the first symptom is an unrelated failure. Deliberately local files rather than an upload to GitHub: kiln keeps them where the build happened, and a task that wants them elsewhere can rsync them.

---

## Migrate one private repository off Actions end to end

The proof, and the thing that will find what the feature list missed. Pick one private repo, express its whole gate in `.warden.yaml` (today kiln's own has vet/test/lint while Actions additionally runs the coverage gate, nox, build, examples, goreleaser check and govulncheck), point a kiln box at it with `kiln watch --every 1m`, require the Kiln checks in branch protection, and delete the workflow file. Write down what broke. Explicit non-goals: GitHub Pages deploys stay in Actions (a deployment belongs with RollOps), and npm publishing of public packages stays in Actions because npm provenance can only be minted from GitHub Actions or GitLab CI on cloud runners — though npm issues no provenance for private repositories at all, so private packages lose nothing by moving.

---
