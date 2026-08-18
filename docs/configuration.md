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

publish:                          # a LIST of artifacts
  - kind: image                   # image | binaries (default: image)
    image: ghcr.io/owner/name     # required for kind: image
    tags: [sha, latest]           # sha + at least one of latest|semver
    sign: cosign                  # only accepted value
    platforms: [linux/amd64]
    dockerfile: Dockerfile
    context: .

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
