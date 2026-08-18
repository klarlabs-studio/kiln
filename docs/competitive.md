# Competitive landscape

*Verified 2026-08-18 against each project's own documentation and the GitHub
API. Star counts move; the structural claims are what this document is for.*

Kiln's README says what it is not — not an Actions clone, not a `runs-on`
worker, not CD. This document says what it is up against, and where an honest
operator would pick something else.

The category is crowded and old. Kiln is not competing on "runs a build in a
container" — that was solved a decade ago by tools with plugin ecosystems it
will never match. It competes on one narrow claim: **the artifact leaves with
two independently signed statements attached, and neither of them is kiln
vouching for somebody else's work.**

---

## The matrix

| | Category | Runs the build | Signs the artifact | Emits SLSA provenance | Carries an *independent* source verdict | Runtime it needs | Licence / stars |
|---|---|---|---|---|---|---|---|
| **Kiln** | build + attest | yes | cosign | SLSA v1, in-toto | **yes** — warden's DSSE envelope, attached unmodified | a box with docker + git | MIT / new |
| GitHub Actions + [Artifact Attestations](https://github.com/actions/attest-build-provenance) | hosted CI + attest | yes | Sigstore (Fulcio, short-lived certs) | SLSA build provenance | no | GitHub-hosted or self-hosted runners | MIT action / GitHub-operated |
| [slsa-github-generator](https://github.com/slsa-framework/slsa-github-generator) | attest on Actions | via reusable workflows | Sigstore | SLSA, targets L3 | no | GitHub Actions | Apache-2.0 / 591 |
| [Tekton Pipelines](https://github.com/tektoncd/pipeline) + [Chains](https://github.com/tektoncd/chains) | k8s-native CI + attest | yes | x509 / KMS / cosign | SLSA v1, automatic | no | Kubernetes | Apache-2.0 / 9.0k + 274 |
| [Jenkins](https://github.com/jenkinsci/jenkins) | general CI | yes | plugin, if you wire it | plugin, if you wire it | no | JVM + agents | MIT / 26.5k |
| [Woodpecker CI](https://github.com/woodpecker-ci/woodpecker) | general CI | yes | a step you write | a step you write | no | server + agents | Apache-2.0 / 7.7k |
| [Concourse](https://github.com/concourse/concourse) | general CI | yes | a step you write | a step you write | no | server + workers | Apache-2.0 / 7.9k |
| [Harness Open Source](https://github.com/harness/harness) (ex-Gitness, absorbed Drone) | forge + CI | yes | a step you write | a step you write | no | server | Apache-2.0 / 38.0k |
| [Argo Workflows](https://github.com/argoproj/argo-workflows) | k8s workflow engine | yes | a step you write | a step you write | no | Kubernetes | Apache-2.0 / 16.9k |
| [Dagger](https://github.com/dagger/dagger) | programmable pipelines | yes | a function you write | a function you write | no | docker/BuildKit | Apache-2.0 / 16.2k |
| [Gitea](https://github.com/go-gitea/gitea) / Forgejo Actions | self-hosted forge + CI | yes | a step you write | a step you write | no | server + runners | MIT / 57.5k |
| [Chainloop](https://github.com/chainloop-dev/chainloop) | evidence store | **no** | Sigstore, over collected evidence | consumes and stores | policy verdicts it evaluates itself | control plane (k8s) or SaaS | Apache-2.0 / 581 |
| [GoReleaser](https://github.com/goreleaser/goreleaser) | release builder | yes, binaries | cosign, if configured | via cosign | no | a box | MIT / 16.0k |

"A step you write" is not a slight — it is the accurate answer. Every general
CI system can invoke cosign, and most supply-chain guides tell you to. The
question the matrix is really asking is what happens when nobody writes that
step, which is the normal case: the artifact ships unsigned and the pipeline
reports green.

---

## The one that actually threatens us

**GitHub Artifact Attestations.** One line of YAML in a workflow already
running, signed by Sigstore with short-lived certificates, verified with
`gh attestation verify`, no infrastructure, no key management, nothing to
operate. For a public repository on GitHub Actions, this is strictly better
than adopting kiln for provenance, and pretending otherwise would be a lie an
operator discovers in ten minutes.

The crack it leaves is specific and worth naming precisely: **artifact
attestations are available in public repositories on all plans, but in private
repositories only on GitHub Enterprise Cloud.** A private repo on Free, Pro or
Team gets nothing. That is the operator kiln can serve honestly — private
source, a build box they already own, no appetite for the Enterprise Cloud
bill.

It is also worth being clear that GitHub attests only what GitHub built. The
attestation says "this artifact came out of this workflow." It does not carry
anyone else's verdict about the commit, because there is no one else in the
picture.

## The category substitute

**A self-hosted forge with Actions-compatible runners** — Gitea or Forgejo —
is the real substitute for the operator kiln targets, in the way Dokploy is the
substitute for RollOps. It answers "where do I build this privately" by owning
the forge, and once it does, an Actions-shaped workflow is the obvious place to
put a cosign step. The counter-argument is not that it cannot sign; it is that
signing remains a step somebody has to write, get right and keep right, and
that nothing checks whether they did.

## Adjacent, not competing

- **Chainloop** is an *evidence store*, not a builder. It attaches to whatever
  CI you have, collects attestations, SBOMs, VEX and SARIF, evaluates Rego
  policies and signs the result. If kiln ever needs somewhere to put evidence
  at organisational scale, this is the shape of the answer, not a rival to it.
- **cosign** and **GoReleaser** are dependencies. Kiln shells out to both and
  invents neither a signing scheme nor a release language, exactly as it reads
  `.warden.yaml` rather than inventing a check language.
- **Tekton Chains** is what kiln would be if kiln required Kubernetes. It signs
  every TaskRun automatically and emits SLSA v1 without anyone writing a step —
  the correct design, for people who already run a cluster. Kiln's whole
  premise is the operator who does not.

---

## What nobody in the table does

Every provenance tool above attests **its own work**: this build produced this
artifact from this commit. None of them carry a *second authority's signed
verdict about the source* alongside it.

Kiln does: warden signs a Verification Summary Attestation with its own ed25519
note key, kiln attaches that DSSE envelope to the artifact **without
re-signing it**, and RollOps verifies the build provenance against kiln's key
and the source verdict against warden's — then checks that both name the same
commit.

The distinction is not decorative. A pipeline that summarises its own gate is
asking to be trusted about whether it ran; the summary is worth exactly the
build platform's word. Carrying the gate's own signature means an auditor can
verify the source verdict without trusting the builder at all, which is the
difference between a provenance chain and a provenance claim.

If a competitor adds this, the differentiator is gone. It is not a hard feature
to copy — it is a design decision none of them have had a reason to make,
because none of them have a separate source gate to carry.

## Where kiln has no answer

Stated plainly, because a landscape document that only flatters its subject is
worthless:

- **Ecosystem.** Jenkins has thousands of plugins. Kiln has `.kiln.yaml` with
  two artifact kinds. Anything not `docker build` or GoReleaser is not a
  supported use case.
- **Build features.** No matrix builds, no distributed caching, no macOS or
  Windows workers, no fan-out. Actions, Concourse and Dagger all beat it here
  without trying.
- **Community.** Every project in this table has years and thousands of stars.
  Kiln has neither, and "trust our supply-chain tool" is a hard sell from a
  project with no history.
- **Nothing forces adoption.** For the public-repo majority, GitHub's free
  attestations already close the gap kiln exists to close.

The winnable operator is narrow and real: **private source outside GitHub
Enterprise Cloud, an existing build box, and a source gate whose verdict they
want carried rather than paraphrased.** Everyone else is better served by
something in this table, and the honest move is to say so.

---

## Refresh

Re-verify before quoting externally, and at least every six months. Two items
in particular:

- GitHub's private-repo plan restriction on artifact attestations is the load-
  bearing fact in the positioning above. If it is lifted, most of this document
  needs rewriting.
- Whether any general CI system has made artifact signing a default rather than
  a step you write.
