# Operating Kiln

## The box

Kiln runs other programs. A machine that runs Kiln needs, on `PATH`:

- `git` — always
- `warden` — always
- `docker` — if an `image` artifact publishes
- `goreleaser` — if a `binaries` artifact publishes
- `cosign` — if anything publishes at all
- `nox` — only with `prove.nox: true`

It also needs a checkout of the repository and credentials for the registry
(`docker login`) and the forge (`GITHUB_TOKEN`).

Check all of it at once:

```bash
kiln doctor
```

`doctor` runs no gate, builds nothing and pushes nothing, so it is safe on a
busy box at any time. It reports `ok`, `warn` and `FAIL`; only `FAIL` exits
non-zero (3).

A `warn` is usually a deliberate choice — no token on a laptop, no trusted keys
on a box that always re-proves. Read them once and then ignore the ones you
meant.

## Unattended

The daemon-less default is cron:

```cron
*/5 * * * *  cd /srv/glossa && /usr/local/bin/kiln watch --once >> /var/log/kiln.log 2>&1
```

Or a process, if you would rather supervise one:

```bash
kiln watch --every 5m
```

One process can watch a fleet, which beats N cron entries when the boxes share
a docker daemon:

```bash
kiln watch --every 5m --repos '/srv/*'
kiln watch --once --repos /srv/api,/srv/worker
```

Repositories tick in sequence. The expensive parts of a tick already saturate
a build box, so running four concurrently makes all four slower and the output
unreadable. One repository failing does not stop the others — that is the
whole reason to run a fleet from one process rather than accept a single point
of failure.

`--every` runs its first tick immediately — an operator starting a watcher
wants to know now whether it works, not in five minutes.

### What a tick does

1. `git fetch` the tracked branch (fatal if it fails)
2. `git fetch +refs/pull/*/head:refs/kiln/pr/*` (not fatal — a non-GitHub
   remote simply has none)
3. `git fetch +refs/tags/*` (not fatal)
4. Ask GitHub which open pull requests are same-repo; without a token, every
   one is a fork
5. Drop any job whose SHA **and** ref already have a *succeeded* run
6. Execute the rest

Only a success suppresses a rebuild, so a transient failure is retried on the
next tick rather than wedging the ref forever. One failing job does not stop
the others — otherwise anybody who can open a pull request could halt the
pipeline.

A closed pull request stops being built: GitHub removes `refs/pull/N/head`, and
the pruning fetch carries that through.

### Seeing what it would do

```bash
kiln watch --once --dry-run
```

Runs nothing, prints the job list. Safe even on a box with no gate installed.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | fine |
| 2 | the gate rejected the change, or a job failed |
| 3 | configuration or toolchain — a broken machine |
| 64 | usage |
| 75 | another kiln holds this repository |

2 and 3 are separated on purpose: 2 needs a developer to fix their code, 3
needs an operator to fix a machine. Alert on 3.

## Concurrency

Kiln takes an exclusive `flock` per repository at `.kiln/lock`. It is advisory,
held on an open descriptor, and released by the kernel when the process exits
— including a SIGKILL — so a crashed build never leaves a repository wedged.

| Caller | Busy repository |
|---|---|
| `kiln run` | refuses, exit 75, naming the holder |
| `kiln watch` / `poll` | skips, exit 0 — overlap is expected under cron |
| `kilnd` | waits, bounded by the build timeout |
| `--dry-run`, `status`, `doctor`, `verify` | never takes the lock |

`--every` locks per tick, not for the loop's lifetime, so a long-lived watcher
does not shut out an operator's `kiln run` between ticks.

If you need to know who holds it: `cat .kiln/lock`.

## Timeouts

`KILN_PHASE_TIMEOUT` (default `45m`) bounds the gate and each artifact's
publish independently. Set `0` to disable it for a genuinely enormous build.
An unparsable value falls back to the default rather than removing the bound.

A timeout exits 3, not 2: it is a machine problem, in the same bucket as a
missing toolchain.

## Running kiln as a box

A box is a machine that already has your toolchain on it. Usually the one you
are typing on.

```bash
kiln login          # a token, stored in the OS keychain
kiln box install    # a schedule that ticks this repository
```

That is the whole thing. `box install` writes a launchd agent on macOS or a
systemd user timer on Linux, loads it, and ticks immediately so you find out
now rather than in five minutes. `kiln box status` says whether it is running
and when it last ticked; `kiln box uninstall` removes it and leaves your ledger
alone.

No token in the unit file — kiln reads the one `kiln login` stored. No
container image to maintain, because the schedule runs as you, with your PATH,
using the same `warden`, `go` and `golangci-lint` you use by hand.

Two things the installer handles that are easy to get wrong on your own:

- **PATH.** A launchd agent inherits `/usr/bin:/bin:/usr/sbin:/sbin` and
  nothing else. The first box installed without pinning PATH found thirteen
  commits and failed all thirteen with "warden is the source gate and kiln
  cannot pass a commit without it". The unit carries the PATH you installed
  with, so a tool installed somewhere new later needs `kiln box install` again.
- **Keychain prompts.** A background job reading the login keychain pops a
  dialog unless the reading binary is on the item's access list. `kiln login`
  puts it there, so the tick does not hang behind a window nobody is looking
  at.

### Is a box worth it?

Measure before you migrate. GitHub bills Actions per job-minute, and the
question is not "how much does CI cost" but "how much of it is Linux minutes on
private repositories", because that is the only part a box replaces. Public
repositories are free and should stay where they are — the runners cost nothing
and artifact attestations are free there too.

```bash
gh api "/orgs/<org>/settings/billing/usage?year=2026&month=8" |
  jq -r '.usageItems[] | select(.product=="actions")
         | [.netAmount, .quantity, .sku, .repositoryName] | @tsv' |
  sort -rn | head
```

That gives net cost per repository per SKU. Three things to look for:

**macOS and Windows minutes.** They bill at roughly ten and two times Linux. A
bill dominated by them is not a bill a Linux box fixes.

**Concentration.** CI spend is usually a handful of repositories. Migrating the
top two is most of the saving and a fraction of the work.

**Whether the minutes are waste.** Before moving anything, check that the
repository cancels superseded runs (`concurrency` with
`cancel-in-progress`) and does not trigger on both `push` and `pull_request`
for the same commit. Moving waste onto your own hardware is still waste.

Then price the other side honestly. A box is a fixed monthly cost and a serial
one: Actions starts every job at once, a box runs them in sequence behind its
lock, so feedback gets slower even when the machine is big enough. You also own
the toolchain — every `npm ci`, database and browser your gates need has to be
on that machine, and a gate that quietly stops running is worse than the bill.

Per-workflow attribution, when you need to know which one to fix:

```bash
gh api "repos/<org>/<repo>/actions/workflows/<id>/runs?status=completed&per_page=5" \
  --jq '.workflow_runs[].id' |
while read run; do
  gh api "repos/<org>/<repo>/actions/runs/$run/jobs" \
    --jq '[.jobs[] | select(.completed_at != null)
           | ((.completed_at|fromdate) - (.started_at|fromdate))] | add'
done
```

Job durations rather than `/timing`, whose `billable` block reports zero on
some plans.

### Give the box its own clone

Point the box at a checkout nothing else touches — `/srv/repos/<name>`, or
anywhere outside the tree you work in:

```bash
git clone git@github.com:you/app /srv/repos/app
cd /srv/repos/app && kiln init && kiln box install
```

Kiln itself is careful with a shared checkout: every phase runs in a detached
worktree, and a repository lock keeps two ticks apart. What it cannot do is
keep *other people* out. A working copy is a place where branches get checked
out, merged branches get deleted and `git reset --hard origin/main` happens —
by you, by your editor, or by an agent finishing a pull request. A tick that
started before one of those and finished after it was gating a commit that is
no longer what the branch means.

Nothing is corrupted when that happens, because the tick works from its own
worktree. What you get is a confusing run: a gate against a commit nobody is
looking at any more, or a discovery pass that finds refs which moved a second
later. On a dedicated clone the only thing moving refs is the fetch kiln does
itself.

### Somewhere other than your laptop

Same two commands on any machine you can log into — a VPS, a NAS, an old Mac
mini. What matters is that the toolchain your gate invokes is installed there,
not what the machine is.

A Kubernetes CronJob is possible and is the *hard* path: k3s runs containerd,
so there is no docker for `kind: image` builds, and you would be maintaining a
container image carrying your whole toolchain — the thing GitHub's runner image
was giving you for free. Worth it for a fleet; not worth it for a first box.

### Watching several repositories

One schedule per repository, or one that watches a directory:

```bash
kiln watch --repos /srv/repos/* --every 5m
```

Overlap is handled: a tick that finds a repository locked exits 75 and says
nothing, which is why a five-minute schedule over a twenty-minute build is
safe.

Failure is handled too, and it is the one that bites on a real box. A commit
whose gate fails waits fifteen minutes before the next attempt, then thirty,
then an hour, and stays hourly. Without that, an open pull request with a
broken test is re-gated every tick for as long as it stays open — 205 failed
runs in one afternoon, the first time this ran for real.

### What the token needs

`kiln login` prints this and links a pre-filled form, so it is here only for
reference: **Commit statuses: write**, **Contents: read** (write if a task
opens pull requests), **Pull requests: write** for the same, **Metadata:
read**. Scope it to the repositories kiln watches, and give kiln its own token
rather than your personal one — it runs unattended and executes
repository-authored commands.

## Checks, and what your token can post

Kiln reports each phase and each task under a name branch protection can
require: `Kiln / Prove`, `Kiln / Publish`, `Kiln / <task>`.

**Which API carries them depends on the token.** Check runs — the rich kind,
with a body — can only be created by a **GitHub App**. Inside Actions that is
invisible, because `GITHUB_TOKEN` there is an app installation token. A
personal access token gets `403 You must authenticate via a GitHub App`, so
kiln falls back to commit statuses: same names, same gating power in branch
protection, a one-line description instead of a body.

If you want the richer output, register a GitHub App for your org, install it
on the repositories kiln watches, and give kiln an installation token.

## Disk

Each watch tick reaps worktrees left by killed runs — directories under the
temp dir carrying kiln's prefix, older than 24 hours, that nobody is building
in. Nothing else collects them, and a box building all day for months otherwise
fills up quietly.

"Nobody is building in" is an `flock` a run holds on its own checkout for as
long as it lives; the kernel drops it when the process dies, however it dies.
That is what makes the reaper safe on a box running two pipelines out of two
checkouts: git can only list the worktrees of the repository it was pointed at,
so a neighbour's live tree looks exactly like abandoned leavings to it, and it
would have deleted one mid-build after a day. It is also what makes the reaper
work at all — `git worktree prune` only drops entries whose directory has gone,
so a killed run leaves one registered, and a reaper trusting that listing would
treat the very leavings it exists for as live forever.

The ledger self-caps at 500 runs.

Each tick also prunes what docker holds, which on a busy box is the larger
number by some way — images are gigabytes and build cache is usually several
times that again. `docker system df` will tell you the split on yours.

**Local only.** A deleted local image is a re-pull away because the registry
holds the durable copy, and that recoverability is the whole reason this can
run unattended. Kiln never deletes from a registry and offers no flag for it:
that is unrecoverable, and it would break the rollback RollOps exists to
perform. Registry retention belongs in your package settings, with a window
wider than your longest plausible rollback.

Within "local", three rules keep it narrow:

- only repositories the pipeline publishes — never `docker image prune -a`,
  which would take every unused image on a shared daemon
- only the immutable `sha-` tags kiln generates
- never the image a moving tag currently points at, even under a sha tag,
  since the same image usually carries both

```yaml
publish:
  - kind: image
    image: ghcr.io/owner/name
    keep: 10        # sha-tagged builds retained locally; 0 never prunes
```

Build cache is a property of the box rather than of a pipeline, so it is an
environment setting: `KILN_BUILD_CACHE_MAX_AGE`, default `168h`, `0` to leave
it alone. It is the one thing here kiln did not create — the daemon's cache is
shared with every other build on the machine — but it is derived, so the worst
outcome of being wrong is somebody's next build being slower.

```bash
kiln prune --dry-run          # what would go
kiln prune --keep 5           # override the pipeline
kiln prune --cache-older-than 24h
```

Retention orders newest-first and breaks ties on the tag name. That tiebreak
matters more than it looks: a reproducible image — anything `FROM scratch`, or
built with `SOURCE_DATE_EPOCH` — reports 1970 as its creation time, so a whole
repository can tie, and without a total order `--dry-run` would name one image
while the real run removed another.

## The ledger

`.kiln/state.json`, or `KILN_DB`. It holds the last 500 runs.

It is runtime bookkeeping, not state. Git is the desired state; deleting the
ledger costs you a duplicate build, never correctness. If it is ever corrupt,
the error says so and tells you to delete it.

A relative `KILN_DB` resolves inside the repository, not the working directory,
so a cron entry that `cd`s somewhere else does not quietly keep a second,
always-empty ledger and rebuild everything every tick.

```bash
kiln status              # the latest run
kiln status --list 10    # the last ten
kiln status <run-id>     # one run
kiln status --json       # for scripting
```

A run still in `proving` hours later shows as `proving (abandoned)`. `kiln run`
is a one-shot process, so a non-terminal phase means the process died.

## Logs

Bolt JSON on **stderr**, always. Stdout belongs to command output and, under
`kiln mcp serve`, to the JSON-RPC stream — a stray log line there takes the
session down.

```bash
KILN_LOG_LEVEL=debug kiln watch --once 2>&1 | jq .
```

## Dry runs

```bash
KILN_DRY=1 kiln run --sha HEAD --event push --ref refs/heads/main
```

Proves for real; plans the publish instead of performing it. Reports a
placeholder digest and `signed: false`, and the `Kiln / Publish` check
concludes **neutral** rather than success, so a rehearsal cannot be mistaken
for a real artifact on a pull request page.

## kilnd

Optional. Cron stays the default; run this only if you already run processes.

```bash
export KILN_TOKEN=$(openssl rand -hex 32)
export KILN_WEBHOOK_SECRET=$(openssl rand -hex 32)
export KILN_DIR=/srv/glossa
export KILN_ADDR=127.0.0.1:8088
kilnd
```

It refuses to boot without `KILN_TOKEN`. There is no anonymous mode to forget
to turn off.

| Route | Auth | Behaviour |
|---|---|---|
| `GET /healthz` | none | liveness |
| `GET /readyz` | none | 503 if `warden` is missing — alive but useless |
| `POST /v1/run` | Bearer | synchronous; a failed build is 200 with `succeeded: false` |
| `GET /v1/runs/{id}` | Bearer | one run |
| `GET /v1/runs` | Bearer | the ledger |
| `POST /v1/github/webhook` | HMAC | 202, then build in the background |

```bash
curl -sS localhost:8088/v1/run \
  -H "Authorization: Bearer $KILN_TOKEN" \
  -d '{"sha":"HEAD","event":"push","ref":"refs/heads/main"}'
```

Point the GitHub webhook at `/v1/github/webhook`, content type
`application/json`, with `KILN_WEBHOOK_SECRET` as the secret. Subscribe to
**Pushes** and **Pull requests**.

SIGTERM stops accepting connections and then waits up to 30 seconds for
in-flight builds. A build killed midway leaves a half-written registry and a
check that never concludes.

## MCP

```bash
kiln mcp serve
```

Three tools: `kiln_doctor`, `kiln_status`, `kiln_run`. Push and tag runs are
refused unless `KILN_MCP_ALLOW_RUN=1`.

## Troubleshooting

**"required tool missing: warden ..."** — the gate is not installed. This is a
failure, not a skip: a commit whose checks did not run has not passed them.

**"branch changed mid-run; re-push"** — you are on a kiln old enough to invoke
`warden run pre-push` without `--attest-only`. That form is the git hook: it
gates and then pushes, which a detached worktree cannot do and a build box
should not. Upgrade.

**"publish: tag plan for ref ... produces no moving tag"** — `tags: [sha,
semver]` on a branch push. Add `latest`, or route `semver` only to tag events.

**Every pull request shows as a fork** — no `GITHUB_TOKEN`, or the API call
failed. Kiln cannot tell a maintainer's branch from a stranger's, and the
permissive guess would hand over credentials.

**Runs never skip the gate** — no `KILN_TRUSTED_KEYS`. Kiln only skips for a
note signed by a key the operator pinned; an unpinned verify would accept a key
the PR author just generated.

**Nothing appears in GitHub Checks** — no token, or the repository could not be
identified. `kiln doctor` says which.

**`docker buildx build` fails on multi-arch** — the default `docker` driver
cannot build multi-platform. `docker buildx create --use`.

**A worktree was left behind** — should not happen; cleanup runs even on
cancellation. `git worktree prune` clears the bookkeeping if it does.

**"release config does not sign its artifacts"** — `.goreleaser.yaml` has no
`signs:` block, so the release would ship a checksum manifest nobody can
verify. Add a cosign `sign-blob` signer over `artifacts: checksum`; kiln's own
`.goreleaser.yaml` is a working example.

**"a binary release needs a tag"** — a `binaries` artifact ran on a branch
push. goreleaser takes the version from the tag. Leave the kind on its `on:
[tag]` default.

**cosign asks for a browser during a release** — keyless signing wants an OIDC
identity, and a self-hosted box has none. Use a key pair (`COSIGN_KEY`,
`COSIGN_PASSWORD`) on a box that is not a CI provider, or supply an OIDC token.
`KILN_DRY=1` skips signing precisely so a laptop rehearsal does not hit this.
