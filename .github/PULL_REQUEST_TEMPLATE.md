<!-- markdownlint-disable MD013 MD041 -->
Closes #<!-- issue number -->

## Summary

<!-- What does this change and why? -->

## How tested

<!-- Commands run, scenarios covered -->

## Checklist

- [ ] PR title follows [Conventional Commits](https://www.conventionalcommits.org/) (validated in CI)
- [ ] `go test ./... -race` passes
- [ ] `go fmt ./...` and `go vet ./...` are clean
- [ ] No new `gosec` findings (baseline updated if the change is intentional)
- [ ] `docs/swagger.yaml` updated for any handler additions/changes
- [ ] Targets `main`; branch will be deleted after squash-merge

<!-- Do NOT add a Changelog section — release-please generates CHANGELOG.md from the Conventional Commit title. -->
