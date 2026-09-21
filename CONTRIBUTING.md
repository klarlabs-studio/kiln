# Contributing to kiln

Thanks for your interest in improving kiln. This project follows a
straightforward, test-first workflow.

## Getting started

```bash
git clone https://github.com/klarlabs-studio/kiln
cd kiln
go build ./...
make all        # fmt-check, vet, test, build
```

kiln requires **Go 1.25+**. It shells out to `git`, `warden`, `docker`,
`cosign`, `goreleaser` and optionally `nox` — `kiln doctor` reports which of
those a box is missing.

## The pipeline

| Step | Command | What it checks |
|---|---|---|
| format | `make fmt-check` | gofmt-clean |
| vet | `make vet` | `go vet` |
| lint | `make lint` | golangci-lint |
| tests | `make test` / `make race` | unit tests, with `-race` |
| coverage | `make cover` | per-package coverage |
| examples | `make examples-check` | the shipped example configs still parse and plan |
| release config | `make dist-check` | `goreleaser check` |

`make release-check` runs the lot. CI runs the same gate.

## What tests are for here

kiln is a supply-chain tool, and the pattern that has cost this project the
most time is **unit tests passing while the real integration was broken**:
a stubbed `warden` that exits 0 hides the fact that the real one would have
pushed; a mocked `cosign` hides an attestation nested one level too deep.

So:

- **A test must not depend on what happens to be on the developer's PATH.**
  Use the `stubTool` helper. A suite that passes only because you have cosign
  installed is a suite that fails in CI and, worse, passes for the wrong
  reason locally.
- **Prefer a test that fails against the old behaviour.** If a bug fix's test
  passes before the fix, it is testing something else.
- **Round-trip anything that leaves the process.** Attestation shapes, exit
  codes and CLI arguments are contracts with other programs.

## Architecture

- `internal/application/authority` — turns a surface's claim into an
  established trust context and takes the repository lock. New doors call
  this; they do not reimplement membership or fork defaults.
- `internal/application/engine` — the one path a run takes once trust is
  established.
- `internal/domain/trust`, `internal/domain/write`, `internal/domain/isolation`
  — evidence mode, kiln-owned proposal branches, and the event×fork matrix.
- `internal/infrastructure/prove`, `publish`, `verify`, `attest` — the phases
  and the statements they produce.
- `internal/infrastructure/execx` — the subprocess seam.
- `internal/interfaces/cli`, `mcpsrv`, `daemon` — delivery.

Keep the phases ignorant of each other, and keep policy decisions out of the
surfaces. See [docs/intent.md](docs/intent.md).

## Commits & PRs

- **Conventional commits** — `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`,
  `test:`.
- Keep commits atomic; each should pass the pipeline on its own.
- Open a PR against `main`; fill in the template. CI must be green.
- Prefer a proper fix over a workaround, and add a regression test for every
  bug.

## Boundaries

Some things are deliberately not features, and a PR adding them will be
declined however well it is written: an apply/canary/rollback path (that is
RollOps), an Actions runner protocol, a second check language beside
`.warden.yaml`, a generic workflow language (`needs`, `matrix`, `jobs`),
and a registry-touching prune. See
[Boundaries](README.md#boundaries),
[`docs/intent.md`](docs/intent.md) and
[`docs/competitive.md`](docs/competitive.md) for where kiln deliberately does
not compete.

## Reporting bugs / security

- Bugs and features: open an issue (templates provided).
- Security vulnerabilities: **do not** open a public issue — see
  [SECURITY.md](SECURITY.md).
