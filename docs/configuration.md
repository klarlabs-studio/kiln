# `.kiln.yaml`

Kiln reads three files, and they have different owners.

`.warden.yaml` says what "passing" means. Kiln does not read it — it shells out
to `warden run pre-push --attest-only` and reports what Warden said. If you want to change
which checks run, change that file.

`.goreleaser.yaml` says how binaries are cross-compiled, archived, signed and
released. Kiln does not read it either, beyond one check — see `binaries` below.

`.kiln.yaml` says what to publish and which events route where. That is all it
says. It is deliberately small, and the things it *cannot* express are as much
a part of the design as the things it can.

Every unknown key is a load error. A typo that silently does nothing is worse
than a failure, because it looks like it worked.

---

## Full schema

```yaml
apiVersion: kiln.klarlabs.de/v1   # required, exact
kind: Pipeline                    # required, exact

on:
  pull_request: [prove]
  push:         [prove, publish]
  tag:          [prove, publish]  # omit to inherit `push`

prove:
  from: warden        # only accepted value
  nox: false          # run `nox scan .` after the gate
  materialize: []     # gitignored dirs the gate needs, e.g. [node_modules]

publish:                          # a LIST of artifacts
  - kind: image                   # image | binaries (default: image)
    image: ghcr.io/owner/name     # required for kind: image
    tags: [sha, latest]           # sha + at least one of latest|semver
    sign: cosign                  # only accepted value
    platforms: [linux/amd64]
    dockerfile: Dockerfile
    context: .
    args:                         # docker build arguments, optional
      BIN: api
    secrets:                      # BuildKit build secrets, optional
      node_auth: env://NODE_AUTH_TOKEN

  - kind: binaries
    from: goreleaser              # only accepted value
    config: .goreleaser.yaml
    on: [tag]                     # default for this kind

watch:
  remote: origin
  ref: main
  pull_requests: true
  tags: true
```

---

## `on` — event routing

Two steps exist: `prove` and `publish`. An event maps to a list of them.

`publish` without `prove` is rejected. It would produce a signed artifact with
no gate behind it, which is the exact thing Kiln exists to prevent.

An absent `on:` block defaults to `pull_request: [prove]`, `push: [prove,
publish]`. An absent `tag:` inherits `push`, because a tag is a push that
happens to carry a name and an operator who wrote `push: [prove, publish]`
plainly meant releases to publish too.

Listing `publish` under `pull_request` parses, but the isolation policy
suppresses it at run time and `kiln doctor` warns about it. See
[isolation.md](isolation.md).

## `prove`

`from` exists as a field only so the coupling to Warden is explicit in the
file. `warden` is its only value; anything else is a load error naming
`.warden.yaml`.

`nox: true` runs `nox scan .` in the same worktree *after* the gate passes. If
`.warden.yaml` already runs nox as a step, this makes it run twice — set it
`false`.

A missing `nox` binary with `nox: true` is a prove failure, not a skip.

## `publish`

A list, because one proven commit routinely yields more than one artifact: the
image RollOps deploys and the release binaries a human downloads. Each entry
carries a `kind`, defaulting to `image`.

An entry may narrow the events it applies to with its own `on:`. Two gates must
both pass for it to run — the event has to route to publish at all
(`on.<event>` lists `publish`), and the artifact must not exclude it. That is
what lets an image build on every push while a release happens only on tags.

Fields belonging to the other kind are rejected rather than ignored, for the
same reason unknown keys are: a setting that silently does nothing looks like
it worked.

## `kind: image`

### `image` — any registry, not just GHCR

`image:` is a plain repository reference, and nothing in kiln is specific to
GHCR. Docker Hub, ECR, Artifactory, Harbor or a registry on your own box all
work the same way:

```yaml
publish:
  - kind: image
    image: docker.io/you/app        # Docker Hub
  - kind: image
    image: 1234.dkr.ecr.eu-central-1.amazonaws.com/app
  - kind: image
    image: registry.internal/team/app
```

A name with no host is a Docker Hub repository — `you/app` and
`docker.io/you/app` are the same image — because that is docker's own rule: the
first path element is a host only if it contains a dot or a colon, or is
exactly `localhost`.

**Kiln never logs in.** It uses whatever credentials docker and cosign already
have, so `docker login` is the operator's job and stays out of the pipeline
file. `kiln doctor` reads docker's config and reports, before a build starts,
whether the registries this pipeline pushes to have credentials — the push is
the last thing a publish does, so a missing login would otherwise surface after
the gate has run and the image has been built.

It reports three states, and the third is not a failure: a credential store or
helper keeps logins outside `config.json`, and a CI runner may inject them in a
way this cannot see, so "could not tell" is said plainly rather than guessed at.

**Docker Hub caveat: attestations land in tags, not referrers.** Docker Hub
implements OCI 1.0.1 and has no referrers API, so cosign falls back to its tag
scheme — `sha256-<digest>.sig` and `.att` appear as tags in the repository next
to your images. Everything works, including `kiln verify`; the signatures are
simply visible in the tag list. Registries that implement OCI 1.1 store them as
referrers instead, out of sight.

### `tags`

| Kind | Produces | RollOps reads it as |
|---|---|---|
| `sha` | `image:sha-abc1234` | the immutable digest to pin |
| `latest` | `image:latest` | `imagePolicy.mode: digest` |
| `semver` | `image:v0.2.0` from a tag ref | `imagePolicy.mode: minor` |

`sha` is always required, and so is at least one moving tag. A sha-only list is
rejected at load: RollOps' `imagePolicy` discovers new digests by watching a
moving tag, so a pipeline that produces only sha tags builds artifacts nothing
can ever find.

`semver` reads the ref. On `refs/tags/v0.2.0` it produces `v0.2.0`; on a branch
push it produces nothing and records a note. That means `tags: [sha, semver]`
on a branch push produces no moving tag at all — Kiln fails the run rather than
publish something undiscoverable. Pair `semver` with `latest`, or route
`semver` only to tag events.

Semver build metadata (`v1.0.0+build.7`) is rewritten to `v1.0.0_build.7`,
because `+` is legal in semver and illegal in an OCI tag. Dropping it would let
two different builds collide on one tag.

### `platforms`

One platform uses `docker build` and `docker push`. More than one switches to
`docker buildx build --push`, which needs a `docker-container` builder
(`docker buildx create --use`) — a multi-arch image cannot exist in the local
daemon's image store, so buildx builds and pushes in a single step.

### `sign`

`cosign`, always. Kiln signs the digest, never a tag: a tag is mutable, and a
signature over one attests to whatever it points at when somebody checks.

## `args` — one Dockerfile, several images

A repository often builds more than one image from a single Dockerfile,
differing only by a build argument. `args` is how you say that:

```yaml
publish:
  - kind: image
    image: ghcr.io/owner/senat-api
    dockerfile: deploy/Dockerfile
    tags: [sha, latest]
    args: {BIN: api}
  - kind: image
    image: ghcr.io/owner/senat-runtime
    dockerfile: deploy/Dockerfile
    tags: [sha, latest]
    args: {BIN: runtime}
```

A **map, not a list**, so an argument given twice is an error YAML catches for
free.

There is deliberately **no passthrough form** — no `args: [FOO]` meaning "take
FOO from the environment". A build whose output depends on the box's
environment is not reproducible from the commit, and reproducibility from the
commit is the entire claim kiln makes about an artifact.

The arguments are **recorded in the provenance**, under
`buildDefinition.externalParameters.buildArgs`. They have to be: two images
built from one commit and one Dockerfile are otherwise indistinguishable in
their attestations while their contents differ. Anyone reproducing the build
needs them, which is what `externalParameters` is for.

Flags are rendered sorted by name, so the same inputs always produce the same
command line and the same attestation.

Build args belong to `kind: image`. On a `kind: binaries` entry they are a load
error — goreleaser owns that build.

---


## `secrets` — credentials a build needs, that the image must not keep

A private dependency needs a token to fetch. `args` cannot carry one: a build
argument is recorded in image history, so a token in one is published with the
artifact.

```yaml
publish:
  - kind: image
    image: ghcr.io/acme/web
    dockerfile: web/Dockerfile
    context: ./web
    tags: [sha, latest]
    secrets:
      node_auth: env://NODE_AUTH_TOKEN
```

Each entry becomes `--secret id=<key>,env=<VAR>`, which the Dockerfile reads:

```dockerfile
RUN --mount=type=secret,id=node_auth \
    NODE_AUTH_TOKEN=$(cat /run/secrets/node_auth) npm ci
```

BuildKit keeps the value out of the image and its history by construction, and
kiln sets `DOCKER_BUILDKIT=1` for any build that uses one — `--secret` is a
BuildKit flag, and an older daemon or `DOCKER_BUILDKIT=0` in the operator's
shell would otherwise fail on an unknown argument.

### Why this is not the env passthrough `args` refuses

`args` deliberately has no `--build-arg FOO`-from-the-environment form, because
a build whose *output* depends on the box's environment is not reproducible
from the commit. A secret is the other case: it is permission to **fetch** what
the commit already pins — a package named in `package-lock.json`, a module
behind a proxy — and it never enters the image. The output still follows from
the commit; only the right to fetch it comes from the box.

### `env://` only

The same scheme as the cosign signing key. A literal value would be a
credential committed to the repository, and a file path would tie the pipeline
to one box's layout. Both are load errors, not warnings.

An id containing a comma, an equals sign or a space is rejected too: it reaches
docker as `id=<id>,env=<VAR>`, where either character silently changes what the
flag means.

### Unset variables fail before the build starts

A missing variable reaches BuildKit as an *empty* secret, and what happens next
is the Dockerfile's business — `npm ci` 401s several minutes in with a message
about the registry, while a more forgiving Dockerfile would publish a signed
image built without the credential. Kiln checks first and names the variable.

### Fork pull requests never see them

Publishing is suppressed for untrusted heads by the isolation policy
([isolation.md](isolation.md)), so a fork build has no publish step to carry a
secret into. Nothing here changes that.

### What the provenance records

The secret **ids**, never the values. Which credentials a build needed is part
of how it was produced, and it is the question asked when one is rotated or
leaked: *what did this token build?*

## `kind: binaries`

Delegates to goreleaser. `config` names the release file, `.goreleaser.yaml` by
default; `from` exists so the coupling is explicit in the file, and
`goreleaser` is its only value.

Defaults to `on: [tag]`. goreleaser derives the version from the tag, so a
binary release on a branch push would publish something with a version nobody
can ask for.

Kiln adds exactly one rule of its own, and it is the point of the kind:

**A release config with no `signs:` block is refused.** Kiln reads the file
before it builds anything, and fails with a message naming what is missing.
Among the sibling repos, warden's config signs its checksum manifest and the
others do not — so whether a release could be verified depended on whether
somebody remembered. This turns that from a habit into a property.

The signature covers `checksums.txt`, and the manifest covers every archive by
digest, so one signature verifies the whole release. That manifest's own digest
is what kiln records as the release's identity.

Note on keyless signing: cosign's keyless mode needs an ambient OIDC identity.
A self-hosted build box does not have one, so a real release there wants either
a cosign key pair (`COSIGN_KEY` / `COSIGN_PASSWORD`) or an OIDC token from
somewhere. `KILN_DRY=1` skips the signing step for this reason — the static
`signs:` check still runs, and it is the guarantee.

## `services`

Containers the gate needs beside it — the database a test suite talks to, a
fake API. This is the Actions `services:` equivalent, and it was the one thing
standing between the first migrated repository and leaving Actions.

```yaml
services:
  postgres:
    image: postgres:16
    port: 5432                    # the port *inside* the container
    env:
      POSTGRES_PASSWORD: test
    ready: pg_isready -U postgres
    ready_timeout: 60s
```

The gate and every task get the address:

```
KILN_SERVICE_POSTGRES_HOST=127.0.0.1
KILN_SERVICE_POSTGRES_PORT=54190
```

**The host port is docker's choice, not yours.** A kiln box runs many
repositories; two pipelines that both bound 5432 would collide, and the symptom
would be a test failing for reasons unrelated to the commit. The published port
is read back after the container starts and handed over in the environment. It
binds loopback only — a test database should not be reachable from the network.

**`ready` is polled until it succeeds**, inside the container, before the gate
starts. Without it the gate begins the instant the container does, which is
well before postgres accepts connections; `kiln doctor` warns about a service
that has no probe.

**Teardown is guaranteed** — after the tasks, on failure, on cancellation, and
if a later service fails to start the earlier ones come down before the error
is returned. A leaked container holds a port on a box that is about to try
again on the next tick.

Service addresses reach even a fork pull request's gate. They are ephemeral
loopback ports rather than secrets, and a fork's tests need the database as
much as anyone's.

## `tasks`

The automation that is neither a check nor an artifact: uploading a scan
result, opening a remediation pull request, refreshing a docs site.

```yaml
tasks:
  sarif:
    on: [push, pull_request]
    run: |
      nox scan --format sarif --output nox.sarif
      gh api repos/$GITHUB_REPOSITORY/code-scanning/sarifs -f commit_sha=$KILN_SHA ...

  remediate:
    on: [schedule]
    every: 24h
    run: nox remediate --open-pr
    allow_failure: true

  docs:
    on: [push]
    workdir: site
    run: make build && rsync -a public/ /srv/www/
```

### `keep` — files that outlive the worktree

```yaml
tasks:
  report:
    on: [push, pull_request]
    run: go test -coverprofile=coverage.out ./... && nox scan --format sarif -o nox.sarif
    keep: ["coverage.out", "*.sarif"]
```

The worktree is destroyed the moment the run ends — which is exactly when
somebody wants the coverage report, or the scan output that explains why the
run failed. Matches are copied to `.kiln/runs/<run-id>/<task>/` before the tree
goes, and `kiln status` lists them.

**Kept on failure too**, especially on failure: withholding the log that
explains a failure in the one case it matters would be the wrong way round.

**A glob that matches nothing is reported.** It is nearly always a typo or a
build that did not get far enough, and silence is how somebody discovers a week
later that the report was never kept.

**Patterns cannot escape the worktree**, including through a symlink. The
pattern comes from the repository, so `keep: ["../../.ssh/id_ed25519"]` is
something a pull request can contain; retention writes into a directory the
operator later reads, and must not become a way to lift files off the build
box. Directory matches are skipped rather than walked, so a stray `*` does not
copy the whole checkout.

Retention is bounded at the last 20 runs, for the same reason the ledger caps
itself and the docker prune keeps ten builds: a box that keeps everything
forever fills its disk, and the first symptom is an unrelated build failing.

### `pull_request` — propose what a task changed

A task that edits the worktree can put the result up for review:

```yaml
tasks:
  remediate:
    on: [schedule]
    every: 24h
    run: nox remediate --fix
    pull_request:
      branch: kiln/nox-remediate
      title: "chore(sec): apply nox remediations"
      body: Opened by kiln. Review the diff before merging.
      labels: [security]
      base: main        # optional; the repository default otherwise
```

**Nothing happens when the worktree is clean.** A remediation task that found
nothing to fix must not push an empty commit or open a pull request saying so
— that is how a useful automation becomes noise people filter out, and then
miss on the day it matters. The check says "no changes to propose".

**One pull request, not one per run.** The branch is the identity: a daily task
pushes to the same branch and updates its existing pull request. Labels are
applied when it is opened and not re-applied afterwards, so an operator who
removed one is not fighting the machine every morning.

**The branch is rebuilt from the commit under test**, force-pushed rather than
fast-forwarded. Yesterday's fix should not outlive the code it was fixing.

**A failed task proposes nothing.** Committing whatever a half-finished
remediation left behind would open a pull request full of a partial fix, which
is worse than no pull request at all.

**A task routed to `pull_request` may not open one** — that is a loop with a
write credential in it, and the config refuses to load. An untrusted head is
refused a second time at runtime, for any caller that assembles a request by
hand.

With no `GITHUB_TOKEN` the branch is still pushed and the check says so. The
work is not thrown away; it just needs a human to notice it.

| Field | Meaning |
|---|---|
| `on` | `pull_request`, `push`, `tag`, `schedule` — required |
| `every` | interval for `schedule`, as a duration string (`24h`, `15m`) |
| `run` | the command, executed by `sh -euc` |
| `workdir` | relative to the worktree root |
| `allow_failure` | record the failure, do not fail the run |

A `schedule` task fires from a `kiln watch` tick against the head of the
tracked ref — there is no new commit, nothing is proven and nothing is
published. The last fire time is kept beside the ledger, so the interval
survives a restart, and a box that was off for a week fires each due task once
rather than replaying the backlog.

Each task posts its own GitHub Check, named `Kiln / <task>`, so branch
protection can require one and a red check names the thing that broke.

**A task cannot mint provenance.** That is the line this feature does not
cross: the signed artifacts of a run are exactly what `publish:` produced, and
no amount of task surface can add to them or make an unsigned thing look
signed. A task's blast radius is a check that goes red and whatever the command
itself did.

Tasks run **after** publish, in a disposable worktree pinned to the commit —
never the operator's working copy, for the same reason the gate and the build
are not. Most tasks are about a build that happened; the ones that are not lose
nothing by waiting, and an automation failure never stops an artifact that was
otherwise ready to ship.

One failure does not stop the others. Artifacts are a set — a release whose
image built and whose binaries did not is incoherent — but tasks are
independent errands, and refusing to upload a scan because a docs build broke
would only hide the second problem behind the first.

`KILN_SHA`, `KILN_REF`, `KILN_EVENT` and `KILN_TASK` are exported. They are
named `KILN_` rather than `GITHUB_` on purpose: a task reading `GITHUB_SHA`
would keep working if somebody moved it back into Actions and silently mean
something different — the merge commit rather than the head.

**On an untrusted head the environment is scrubbed**, exactly as it is for the
gate. A task runs repository-authored commands, so a fork pull request that
could read the environment would put every secret on the box one pull request
away.

## `watch`

`remote` and `ref` name the branch a tick follows. `pull_requests` and `tags`
default to `true`; setting either `false` is honoured (they are tri-state
internally, so "absent" and "explicitly false" are distinguishable).

---

## What you cannot write

```yaml
deploy:   { ... }   # load error
apply:    { ... }   # load error
rollout:  { ... }   # load error
canary:   { ... }   # load error
rollback: { ... }   # load error
```

Each of these fails with a message naming RollOps. This is not an oversight
being tracked as a feature request — Kiln produces artifacts and RollOps ships
them, and a `deploy:` key here would be the first crack in that boundary.

There is also no `steps:`, no `jobs:`, no `runs-on:` and no `container:`.
Checks live in `.warden.yaml`; compute is the machine Kiln runs on.

---

## No pipeline at all

A repository with no `.kiln.yaml` gets the default: prove every event, publish
nothing. `kiln doctor` reports it as a warning rather than a failure, because
that is exactly right for a library.

---

## `prove.materialize`

A gate runs in a disposable worktree, not in your clone. Anything gitignored
is therefore absent from it — which is invisible for Go, whose dependencies
come from the module cache, and fatal for Node, whose `node_modules` is
exactly that:

```
test could not run: vitest is not installed in the validation worktree
```

The trap is that the same gate passes when you run it by hand in the clone,
where `node_modules` exists. That reads as flakiness rather than as a missing
file.

Name what has to come across:

```yaml
prove:
  from: warden
  materialize: [node_modules]
```

Entries are copied from the clone into the worktree before the gate runs,
hardlinked where the filesystem allows, and never over a path the commit
itself provides — what is tracked wins over what happens to be lying in the
clone. A named directory that does not exist is skipped rather than failing
the run: a fresh clone where nobody has installed anything yet is not a
pipeline error, and the gate will say so on its own terms.

### It does nothing for a fork

This list is read from the commit being gated, so on a pull request its author
wrote it — and on a fork that author is a stranger. `materialize: [".env"]`
would otherwise hand them whatever sits beside your clone, inside code kiln is
about to execute.

So materialising is governed by the same bit as registry and signing
credentials: a fork gets nothing, and its gate fails with "could not run".
That is the honest outcome, and it is the reason a Node repository cannot be
gated on fork pull requests by a box today. Same-repo pull requests, pushes
and tags are unaffected.
