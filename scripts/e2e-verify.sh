#!/usr/bin/env bash
# End-to-end check of `kiln verify` against REAL cosign and a REAL registry.
#
# Every unit test of the verify path scripts cosign with a fake. That proves
# "given cosign says X, kiln concludes Y" and cannot prove "cosign, run with
# kiln's arguments, says X". This script closes the other half, and found a
# defect on its first run: a refusal rendered as "FAIL signature 0 < 1" with
# the explanation discarded.
#
# Requires docker, cosign and go; skips cleanly without them. Leaves nothing
# behind.
#
#   ./scripts/e2e-verify.sh
#
# SCOPE. Signing uses --tlog-upload=false so a throwaway test never writes to
# the public transparency log. cosign then refuses the signature at verify
# time for lacking a log entry, so the POSITIVE path — a good artifact
# verifying clean — is NOT covered here; that needs a local Rekor. What is
# covered is the direction that matters for trust: kiln refusing what it
# should refuse, for a reason it can state.
set -euo pipefail

REG_PORT=${REG_PORT:-5555}
REG_NAME=kiln-e2e-reg
WORK=$(mktemp -d)
trap 'docker rm -f "$REG_NAME" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

missing=""
for tool in docker cosign go; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if command -v docker >/dev/null 2>&1 && ! docker info >/dev/null 2>&1; then
  missing="$missing docker-daemon"
fi

# Skip on a laptop that lacks the tools; FAIL when the caller says the
# environment was prepared.
#
# The order matters and got this wrong once: the skip used to come first, so a
# CI run whose cosign install had failed would skip every check and report
# green. A skip is a pass as far as CI is concerned, which makes "skipped
# everything" the most dangerous outcome this script has.
if [ -n "$missing" ]; then
  if [ -n "${KILN_E2E_REGISTRY_RUNNING:-}" ]; then
    echo "FAIL: prepared environment is missing:$missing"
    exit 1
  fi
  echo "SKIP: not installed:$missing"
  exit 0
fi

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
go build -o "$WORK/kiln" "$REPO_ROOT/cmd/kiln"

# In CI the registry is a service container that already holds the port, so
# starting a second one just fails on the bind. KILN_E2E_REGISTRY_RUNNING says
# "one is already there, use it and do not clean it up".
if [ -z "${KILN_E2E_REGISTRY_RUNNING:-}" ]; then
  docker rm -f "$REG_NAME" >/dev/null 2>&1 || true
  docker run -d --name "$REG_NAME" -p "$REG_PORT:5000" registry:2 >/dev/null
fi
cd "$WORK"

export COSIGN_PASSWORD=""
cosign generate-key-pair >/dev/null 2>&1
mkdir attacker && (cd attacker && cosign generate-key-pair >/dev/null 2>&1)

printf 'FROM scratch\nCOPY hello.txt /hello.txt\n' > Dockerfile
echo hello > hello.txt
IMG="localhost:$REG_PORT/kiln-e2e"
docker build -q -t "$IMG:v1" . >/dev/null
docker push -q "$IMG:v1" >/dev/null
DIGEST=$(docker inspect --format='{{index .RepoDigests 0}}' "$IMG:v1" | cut -d@ -f2)
REF="$IMG@$DIGEST"

cosign sign --key cosign.key --use-signing-config=false --tlog-upload=false --yes "$REF" >/dev/null 2>&1

fail=0
say() { printf '%s\n' "$1"; }

# refused <name> [args...] — kiln must not report PASS for any link.
refused() {
  local name=$1; shift
  local out; out=$("$WORK/kiln" verify "$@" 2>&1 || true)
  if grep -q "PASS" <<<"$out"; then
    say "FAIL: $name — kiln accepted it"; sed 's/^/    /' <<<"$out"; fail=1
  else
    say "ok: $name"
  fi
}

# says <name> <substring> [args...] — the refusal must carry this wording.
says() {
  local name=$1 want=$2; shift 2
  local out; out=$("$WORK/kiln" verify "$@" 2>&1 || true)
  if grep -qF "$want" <<<"$out"; then
    say "ok: $name"
  else
    say "FAIL: $name — wanted \"$want\" in output"; sed 's/^/    /' <<<"$out"; fail=1
  fi
}

# The one that regressed: the message was "0 < 1", explanation discarded.
says "a refusal explains itself" "transparency log" --key cosign.pub "$REF"

# Never signed at this digest.
says "a tampered digest is refused" "no signatures found" \
  --key cosign.pub "$IMG@sha256:$(printf '0%.0s' {1..64})"

# Signed, but not by the key the verifier trusts.
refused "an untrusted key is refused" --key attacker/cosign.pub "$REF"

exit $fail
