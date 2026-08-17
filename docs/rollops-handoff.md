# The RollOps hand-off

Kiln produces. RollOps ships. This document is the seam between them.

## What Kiln guarantees

A successful publish leaves the registry holding:

- an **immutable** tag, `image:sha-<short>`, for the exact commit
- **at least one moving** tag — `latest`, a semver tag, or both
- a **cosign signature over the digest**, not over any tag

and leaves the run ledger holding the digest, the tag list, the commit, the ref
and whether the gate ran or was satisfied by a Warden note.

That last field matters for audit. The chain is two statements:

1. **Warden's note** on the commit — the configured checks ran and passed.
2. **Kiln's digest** on the run — this image was built from that commit.

MVP records both. An in-toto/SLSA export can be layered on later without
changing either.

## What RollOps consumes

```yaml
# rollops, not kiln
imagePolicy:
  image: ghcr.io/felixgeelhaar/glossa-api
  mode: digest        # follow :latest, resolve to a digest
```

| Kiln tag | RollOps `imagePolicy.mode` |
|---|---|
| `sha-abc1234` | the digest to pin once resolved |
| `latest` | `digest` |
| `v0.2.0` | `minor` |

This is why a sha-only tag list is a load error in Kiln. `imagePolicy`
discovers new builds by watching a moving tag; with only sha tags there is
nothing to watch, and Kiln would be producing artifacts RollOps could never
find.

## Who enforces the signature

RollOps does, at apply time, with `ROLLOPS_COSIGN_KEY`.

Kiln signs; it does not verify at deploy. That split is deliberate. An
enforcement check run by the same component that produced the artifact proves
very little — the point of RollOps holding the key is that a compromised Kiln
cannot ship an unsigned image, because the thing that ships is not Kiln.

The corollary is that a missing `cosign` on the build box is a **publish
failure** in Kiln, not a warning. A pipeline that quietly stopped signing would
hand RollOps an artifact it refuses, and the operator would find out at deploy
time instead of build time.

## Checks as a synchronisation point

`Kiln / Prove` and `Kiln / Publish` are contract names. Branch protection rules
require checks by name, and RollOps' PR writeback waits on them. Renaming one
silently unblocks every protected branch that was waiting for it, so a change
needs a migration note, not just a commit.

Pull requests never get a `Kiln / Publish` check. There is nothing to publish
from a proposal.

A prove satisfied by a trusted Warden note concludes **success**, not
`skipped` — the commit is gated, and a protection rule must be satisfied. The
summary explains which key vouched for it.

## What Kiln will never send

No apply. No canary. No drift detection. No rollback. No deployment target of
any kind, and a `deploy:` key in `.kiln.yaml` is a load error rather than a
feature request.

The interface between the two products is a signed digest in a registry and
nothing else. Widening it would make both of them worse.
