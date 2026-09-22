# Isolation

Proving a commit means executing code from that commit. On a pull request from
a fork, that code was written by a stranger. Everything in this document
follows from taking that sentence seriously.

## The matrix

| Event | Fork | Secrets | Publish | Provenance skip | Confine |
|---|---|---|---|---|---|
| `pull_request` | yes | no | no | no | yes |
| `pull_request` | no | no | no | yes | no |
| `push` / `tag` | — | yes | yes | yes | no |

This lives in `internal/domain/isolation`, as a pure function of two inputs
with no I/O. It can be exhaustively tested, and it is.

## The caller states intent; the policy decides

The engine consults the policy *after* the caller has said what it wants. A
`.kiln.yaml` on a fork head that lists `publish` under `pull_request` is
overruled, not obeyed. So is an MCP agent asking for a publish, and so is an
HTTP client.

That inversion is why new surfaces are cheap: they cannot express a way around
the rules, because the rules are not applied at the edge.

## Surfaces are not security boundaries

CLI, MCP, `POST /v1/run`, the webhook, watch and schedule all ask for work.
They do not define reality.

A JSON body that says `{ "event": "push", "fork": false }` is a claim. The
application layer resolves the SHA against the watched ref (or a tag this
repository knows) before the engine sees a publishable event. A webhook
delivery is different: the HMAC is the evidence, and the parsed job is
already established.

A pull request without a number, or whose forge lookup fails, is a fork.
`--fork` and `"fork": true` are a floor, never a ceiling.

Repository locking lives on the same path. MCP, CLI and HTTP cannot bypass
it by using a different door.

## Why a pull request never publishes

Not just fork pull requests — any pull request.

A pull request is a proposal. RollOps deploys from branches and tags, so an
image built from an unmerged head is an artifact that nobody should ever be
able to ship. Building one is at best a waste of a registry slot and at worst a
way to smuggle a deployable artifact past review.

## Why a fork gets nothing at all

A fork pull request's head contains attacker-authored code that Kiln is about
to execute. Two consequences:

**No secrets.** The gate runs with a scrubbed environment. `internal/infrastructure/execx`
drops anything matching `TOKEN`, `SECRET`, `PASSWORD`, `CREDENTIAL`, `API_KEY`,
`AUTH` and friends, plus a named list covering `GITHUB_TOKEN`, registry
credentials, cosign material, `SSH_AUTH_SOCK` (agent forwarding is a live
credential, not a value), common connection forms (`DATABASE_URL`, `DSN`,
`CONNECTION_STRING`, `*_PEM`, `*_URI`), and paths to credential files
(`KUBECONFIG`, `NETRC`, `GNUPGHOME`, `AWS_SHARED_CREDENTIALS_FILE`). Ordinary
variables — `PATH`, `HOME`,
`CI` — survive, and `KILN_ISOLATED=1` is added so a repository's own checks can
tell they are running without credentials and skip an integration test rather
than fail confusingly.

This is a denylist, deliberately. An allowlist would be tighter but would break
every build that needs a variable Kiln has never heard of, and an operator who
works around the isolation is worse off than one whose unusually-named secret
slipped through.

**No provenance skip.** The warden note on a fork head was authored on that
same untrusted head. Kiln does not even run `warden verify` for a fork — there
is no verdict it could produce that would change the answer, and not running is
one less thing a hostile head can influence.

## Fail closed on "I don't know"

Kiln asks GitHub whether a pull request head lives in the same repository.
Every way that question can fail resolves to **fork**:

- no `GITHUB_TOKEN` → fork
- the API call failed → fork
- `head.repo` is null (the fork was deleted) → fork
- `kiln run --event pull_request` with no `--pr` number → fork
- `POST /v1/run` with `event: pull_request` and no `pr` → fork

The conservative guess costs a re-prove. The permissive guess hands a stranger
the operator's registry credentials.

`--fork` is a floor, never a ceiling: passing it forces untrusted handling, and
nothing — not the API, not a token appearing later — turns it back off.

## The provenance skip

Skipping the re-prove requires **both** conditions:

1. The policy permits it (see the matrix).
2. `warden verify --commit <sha> --require-signed --key $KILN_TRUSTED_KEYS`
   exits zero.

`KILN_TRUSTED_KEYS` is read from the operator's environment and never from the
repository. With no keys pinned, Kiln does not skip at all — it does not run an
unpinned verify, because `warden verify` without `--key` will happily validate
a note signed by any key, including one the pull request author generated five
minutes ago.

A skipped prove concludes the `Kiln / Prove` check as **success**, not
`skipped`. The commit *is* gated — by the note — and a branch protection rule
waiting on that check must be satisfied, or every provenance skip would block a
merge. The check summary says how it was satisfied, and the run ledger records
`prove_skipped: true`, so an auditor can tell which runs executed the checks
and which inherited them.

## Worktrees

Prove and publish each check the commit out into a fresh temp directory with
`git worktree add --detach`, and remove it afterwards — including when the run
fails, panics, or is cancelled (cleanup runs on a context detached from the
cancelled one, because that is exactly when it matters).

Without this, an uncommitted edit sitting in the operator's checkout would end
up inside a signed image, and the digest handed to RollOps would attest to a
commit that never contained the code it shipped.

A worktree is still not a sandbox. It isolates source state from the
operator's dirty checkout. On a **fork** pull request, Kiln additionally
asks the kernel to Landlock-restrict the gate and tasks to that worktree
plus toolchain paths (`/usr`, the module cache, `/proc`, …). `/dev` is
writable so a child can use `/dev/null`; it is not a secret store.
`/var/run/docker.sock` and the operator's home are not in the grant.
Network is not confined. Landlock denies open, not `stat` — a path
outside the grant can still be listed. `/proc` is granted so the
toolchain can run; that is a known hole (`/proc/self/root`), which is
why this is not a sandbox. Kiln records `KILN_CONFINED=landlock` on
the child only when the LSM actually applied. If the kernel has no
Landlock, the run continues with environment scrubbing only and does
not claim to be confined. `KILN_CONFINE=off` disables the request;
`KILN_CONFINE=required` fails the fork run when Landlock cannot apply.

## The webhook

A delivery must carry a valid `X-Hub-Signature-256`. Only the `sha256` scheme
is accepted — GitHub still sends a legacy `sha1` header, and accepting it would
let an attacker downgrade to a broken MAC by choosing which header to present.
Comparison is constant-time.

A server with **no** secret configured rejects every delivery with the same 401
as a forged one. An unsecured webhook endpoint is one that lets anybody on the
internet make a machine build and sign a commit of their choosing.

## The MCP surface

`kiln_run` with `event=pull_request` is always allowed: it produces no artifact
and holds no credential. `event=push` and `event=tag` are refused unless the
operator set `KILN_MCP_ALLOW_RUN=1`, because those are the runs that publish
and sign something real.

The refusal message names the variable. An agent told "internal error" has no
move except to give up or retry identically.

## Proposal writes

A task may force-push only a branch under `kiln/`. `main`, `master`, the
watched ref, and anything that escapes that namespace are load errors.
Kiln may replace its own proposal branches. It may not rewrite
source-of-truth.
