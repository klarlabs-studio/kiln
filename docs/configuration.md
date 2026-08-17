# `.kiln.yaml`

Kiln reads two files, and they have different owners.

`.warden.yaml` says what "passing" means. Kiln does not read it — it shells out
to `warden run pre-push` and reports what Warden said. If you want to change
which checks run, change that file.

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

publish:
  image: ghcr.io/owner/name       # required when anything publishes
  tags: [sha, latest]             # sha + at least one of latest|semver
  sign: cosign                    # only accepted value
  platforms: [linux/amd64]
  dockerfile: Dockerfile
  context: .

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
