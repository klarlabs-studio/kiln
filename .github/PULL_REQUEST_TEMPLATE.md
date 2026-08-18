## What

<!-- What does this change do, and why? -->

## How

<!-- Notable implementation details, trade-offs, alternatives considered. -->

## Checklist

- [ ] `make release-check` passes locally (fmt, vet, test, build, examples, `goreleaser check`)
- [ ] Tests added/updated — and a bug fix's test fails against the old behaviour
- [ ] No test depends on what happens to be on the developer's PATH (use `stubTool`)
- [ ] Conventional commit messages (`feat:`, `fix:`, `docs:`, …)
- [ ] Docs updated (README / `docs/`) if behaviour, config or the published contract changed
