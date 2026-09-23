# Intent

Kiln is a signed-artifact factory. It is not a CI system, a deployment
system, or an Actions-compatible runner.

Its job is the trustworthy handoff from source to artifact:

```
source authority          build authority          deployment authority
     Warden                     Kiln                     RollOps
       │                         │                         │
       │ signed source verdict   │                         │
       ├────────────────────────>│                         │
       │                   build, sign, attest             │
       │                         ├────────────────────────>│
       │                         │   artifact + evidence    │
```

Each component signs only facts it directly established. The next
component verifies those facts rather than reinterpreting them.

## What a successful build leaves behind

An immutable artifact and enough evidence for another machine to
independently answer:

- What source produced this?
- Who established that the source passed policy?
- Did Kiln reproduce that verdict or inherit it?
- Which build policy governed the build?
- Who built the artifact?
- What immutable digest was produced, and is it signed?
- Has any evidence been altered?

Moving tags are discovery. The digest is the identity.

## Four things conventional CI collapses

1. **Source identity.** A SHA supplied by a caller is an identifier, not
   evidence. Event, ref and fork status are derived from the forge and
   the repository, not from a JSON body.
2. **Source authority.** Warden establishes facts about source. Kiln may
   inherit a signed verdict or reproduce the checks. It never converts
   "I trust Warden" into "Kiln performed these checks."
3. **Build authority.** Kiln signs what it observed: artifact A from
   source S under policy P, digest D.
4. **Deployment authority.** That is RollOps. Permanent.

## Operator-owned build policy

`.kiln.yaml` is the operator checkout's file. That is part of the
security model, not an implementation accident.

The source being built and the policy controlling the build are
different objects. Provenance records the policy identity:

```yaml
policy:
  source: operator
  path: .kiln.yaml
  digest: sha256:...
```

Commit-controlled policy is an explicit trust-boundary change, not the
default. The operator must write `policy.from: commit` in the checkout's
`.kiln.yaml`. Then the SHA being built supplies the file, discovery
(`watch`) stays operator-owned, and a fork cannot start the commit's
services. Isolation still suppresses secrets and publish. Absence of
`.kiln.yaml` at that SHA fails the run. Provenance records
`source: commit` and the SHA.

## Evidence completeness

```yaml
evidence:
  source: required      # production: a missing Warden verdict fails publish
  # source: best-effort # adoption: the gap is visible in provenance
```

A box with `KILN_TRUSTED_KEYS` pinned defaults to `required`. Best-effort
exists for migration and must be visible in verification output.

## Surfaces are not security boundaries

CLI, MCP, HTTP, webhook, watch and schedule all reduce to a claim. The
application layer establishes source, event, fork status, repository
exclusivity and capabilities. A new interface should be boring. A new
forge is the same idea: GitHub, Gitea or Forgejo implement the host
port. They do not define trust.

## Repository writes

A task may propose. It may not rewrite source-of-truth. Proposal
branches belong to `kiln/*`. Configuration that names `main` is a load
error, not a force-push destination.

## What Kiln must not become

No Actions clone. No generic CI platform. No deploy/canary/rollback.
No second check language beside `.warden.yaml`. No claim that a worktree
is a sandbox.

A worktree isolates source state. It does not isolate privileges. A fork
child may also be Landlock-restricted to that tree; that is a kernel
restriction, and Kiln only claims it when the LSM actually applied.

## Product test

Every feature must answer: what fact did Kiln uniquely observe that
entitles it to make this statement?

Good: "I observed this build produce digest X."
Bad: "The caller told me this was a trusted push."
