<!-- markdownlint-disable MD013 MD041 -->
Closes #<!-- issue number -->

## Summary
<!-- What does this change and why? -->

## Changelog
<!-- Required: one entry that will be collected into CHANGELOG.md at release time.
     Use the appropriate type: feat | fix | perf | deps | docs | refactor | security -->
- fix:

## Checklist

- [ ] `go test ./internal/... -race` passes
- [ ] `go fmt ./...` and `go vet ./...` clean
- [ ] Filtered coverage stays at or above the 79% floor
- [ ] No new `gosec` findings (or baseline updated with justification)
- [ ] `make swag` run and `docs/swagger.json` committed (if a handler changed)
- [ ] Docs updated (`TSM_*` config, deployment, or feature docs as applicable)
- [ ] PR targets `main` with a Conventional Commit title
- [ ] Remote branch will be deleted after merge
