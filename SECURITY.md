# Security Policy

## Supported versions

kiln is pre-1.0; security fixes land on the latest minor release. Always run
the newest tagged version.

| Version | Supported |
|---|---|
| latest `0.x` | ✅ |
| older | ❌ |

## Reporting a vulnerability

**Do not open a public issue for security vulnerabilities.**

Report privately via GitHub's [private vulnerability
reporting](https://github.com/klarlabs-studio/kiln/security/advisories/new)
(Security → Report a vulnerability), or email **felix.geelhaar@gmail.com** with
`kiln security` in the subject.

Please include:

- affected version (`kiln version`),
- a description and, if possible, a minimal reproduction,
- the impact you foresee.

You'll get an acknowledgement within a few days. Fixes are developed privately,
released, and disclosed once users can upgrade.

## Verifying a release

Every release carries a cosign bundle over the checksum manifest, signed
keylessly by the release workflow, and a CycloneDX SBOM per archive. The
identity is the point — it names the workflow and the tag that produced the
file, so a signature cannot be reused for a build made anywhere else:

```bash
VERSION=$(curl -fsSL https://api.github.com/repos/klarlabs-studio/kiln/releases/latest | jq -r .tag_name)
cosign verify-blob \
  --bundle checksums.txt.bundle \
  --certificate-identity \
    "https://github.com/klarlabs-studio/kiln/.github/workflows/release.yml@refs/tags/$VERSION" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

The identity names the workflow **and the tag**. A hardcoded version would
make every later release look forged — the exact trust failure this command
exists to catch.

## Security model — what kiln does and doesn't guarantee

- **Two authorities, two signatures.** Kiln signs its own build provenance
  (SLSA v1, cosign) and *carries* warden's source verdict without re-signing
  it. A summary re-signed by the builder would attest to the builder; carried
  intact, the source verdict stands on warden's key alone. A consumer verifies
  each claim against the authority that made it, and nothing has to be taken on
  the pipeline's word.

- **The commit join is checked, and it is what makes the chain a chain.** Kiln
  refuses a source summary whose commit is not the commit it built. Without
  that check, a summary for a well-gated commit could travel on an artifact
  built from an ungated one, and both attestations would verify perfectly on
  their own.

- **Nothing leaves kiln unsigned.** A missing `cosign` is a failure before the
  push, not after it, and `kiln` refuses a GoReleaser config with no `signs:`
  block. A publish that could silently produce an unsigned artifact would make
  every downstream guarantee conditional on nobody having made a mistake.

- **A skipped gate needs a pinned key.** Kiln will re-prove a commit unless a
  warden note is signed by a key the operator pinned in `KILN_TRUSTED_KEYS`.
  With no pinned keys there is no skip: `warden verify` without `--key`
  validates a note signed by *anyone*, including the pull request author.

- **Fork pull requests are not trusted, structurally.** The isolation policy is
  a function of the event, not a flag a caller passes. A fork PR never inherits
  a provenance skip and never publishes, and the code that decides this does
  not accept an override.

- **Callers request work; they do not grant authority.** `POST /v1/run`, MCP
  and the CLI supply a SHA and an event claim. Push and tag authority is
  established by membership on a trusted ref. A pull request without a number,
  or whose forge lookup fails, is a fork. A verified webhook is already
  evidence.

- **`.kiln.yaml` is operator-owned.** Routing, services and proposal
  destinations come from the box checkout, not from the commit being built.
  The file's digest is recorded in provenance.

- **Proposal writes stay under `kiln/`.** A task may force-push its own
  proposal branch. It may not rewrite `main` or the watched ref because the
  pipeline named that branch.

- **Source evidence is a policy, not a warning.** With trusted Warden keys
  pinned, a publish that cannot attach the source verdict fails. Best-effort
  is for adoption and is visible in the attestation.

- **Builds run repository-authored commands.** Kiln runs what `.warden.yaml`
  and your Dockerfile/`.goreleaser.yaml` say to run, in a disposable worktree,
  with the permissions of the user running kiln. **The worktree is isolation
  from your working copy, not a sandbox.** Treat those files as trusted code
  and review changes to them accordingly. A fork's prove and tasks are
  additionally Landlock-restricted to the worktree and toolchain paths when
  the kernel supports it (filesystem open is denied; `stat` is not, and
  network stays open). That is a kernel fact, recorded as
  `KILN_CONFINED=landlock` on the child. Without Landlock the child is
  environment-scrubbed only. `KILN_CONFINE=required` refuses the fork run
  in that case.

- **The signing identity is ambient.** Kiln invokes `cosign` and inherits
  whatever key or OIDC identity the environment gives it. Kiln does not manage
  key material, and `kiln doctor` can tell you cosign is installed but not that
  it will be able to sign.

- **The HTTP surface is optional and authenticated.** `kilnd` requires a bearer
  token on every route that does anything, and an HMAC signature on webhooks. A
  missing webhook secret is the same 401 as a forged signature — an
  unauthenticated build trigger is a remote code execution primitive.
  `KILN_TOKEN` is a publish credential: treat a leak as registry-write plus
  signing. JSON callers still cannot manufacture push/tag authority for a SHA
  that is not on a trusted ref. `POST /v1/run` admits one caller at a time
  (429 if another build is already in flight); a leak can still publish, it
  cannot stack builds until the box falls over.

- **MCP is read-only unless you say otherwise.** Agents get `doctor` and
  `status` freely and pull-request proves; push and tag runs are refused unless
  `KILN_MCP_ALLOW_RUN=1`. There is no deploy tool on any surface.
