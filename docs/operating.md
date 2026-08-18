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

## Disk

Each watch tick reaps worktrees left by killed runs — directories under the
temp dir carrying kiln's prefix, older than 24 hours, that git does not list as
live. Nothing else collects them, and a box building all day for months
otherwise fills up quietly.

The ledger self-caps at 500 runs. Docker images are **not** pruned: what is
safe to delete from a registry cache is an operator's call, not kiln's.

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
