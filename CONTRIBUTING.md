<!-- markdownlint-disable MD013 -->
# Contributing to Terraform State Manager — Backend

Thank you for your interest in contributing. Terraform State Manager (TSM) analyzes,
manipulates, and watches Terraform state **where it already lives** and stores encrypted
source and CI credentials. Because it touches infrastructure state and secrets,
contributions that uphold correctness, security, and safe state handling are especially
welcome.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Workflow](#development-workflow)
- [Backend (Go) Standards](#backend-go-standards)
- [Database Migrations](#database-migrations)
- [Adding a New State Source](#adding-a-new-state-source)
- [Testing Requirements](#testing-requirements)
- [Pull Request Process](#pull-request-process)
- [Reporting Security Vulnerabilities](#reporting-security-vulnerabilities)
- [Documentation](#documentation)

---

## Code of Conduct

This project expects all participants to interact with each other professionally and
respectfully. Harassment, discrimination, or disruptive behavior of any kind is not
acceptable. See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

---

## Getting Started

### Prerequisites

- Go 1.26 or later (the toolchain version is pinned in `backend/go.mod`)
- PostgreSQL 14+ (16 recommended)
- Docker (for the image smoke test and the full local stack)

### Fork and Clone

```bash
git clone https://github.com/sethbacon/terraform-state-manager-backend.git
cd terraform-state-manager-backend
```

### Local Setup

```bash
# Resolve dependencies (first run)
make tidy

# Point the binary at the example config and run against a reachable PostgreSQL.
# Migrations run automatically on serve; run them standalone with the migrate subcommand.
make run

# Or step by step:
cd backend
CONFIG_PATH=../config.example.yaml go run ./cmd/server migrate up
CONFIG_PATH=../config.example.yaml go run ./cmd/server serve   # listens on :8080
```

Configuration is layered: built-in defaults < the YAML file (`CONFIG_PATH`) <
`TSM_`-prefixed environment variables (e.g. `TSM_DATABASE_HOST` overrides
`database.host`). A config file is optional — the service runs on defaults plus env
vars alone. See [docs/configuration.md](docs/configuration.md) for the full reference.

A complete dev stack (PostgreSQL + backend + frontend + Keycloak IdP, with seed states)
lives in the [terraform-state-manager-frontend](https://github.com/sethbacon/terraform-state-manager-frontend)
repo's `deployments/` Docker Compose. The frontend UI is a separate React SPA.

> **Encryption key:** `TSM_ENCRYPTION_KEY` protects stored source and CI credentials and
> has **no re-encryption tooling**. Treat it as a secret even locally, and never commit
> a real key. See [SECURITY.md](SECURITY.md).

---

## Development Workflow

The repository is **main-only**: branch from `main` and open your PR against `main`.
There is no long-lived development branch.

### Branch Naming

| Type          | Pattern                  | Example                          |
| ------------- | ------------------------ | -------------------------------- |
| Feature       | `feat/short-description` | `feat/consul-cas-write`          |
| Bug fix       | `fix/issue-description`  | `fix/drift-ingest-dedup`         |
| Documentation | `docs/topic`             | `docs/deployment-guide`          |
| Refactor      | `refactor/area`          | `refactor/statesource-interface` |

### Conventional Commits

PR titles (and commit messages) must follow [Conventional Commits](https://www.conventionalcommits.org/).
PR titles are validated in CI by `amannn/action-semantic-pull-request`.

```text
<type>(<optional scope>): <description>
```

| Type       | When to use                                    |
| ---------- | ---------------------------------------------- |
| `feat`     | New user-facing feature (minor version bump)   |
| `fix`      | Bug fix, including security fixes (patch bump) |
| `perf`     | Performance improvement                        |
| `refactor` | Code restructure, no behavior change           |
| `docs`     | Documentation only                             |
| `deps`     | Dependency updates                             |
| `security` | Security hardening grouped in the changelog    |
| `revert`   | Reverts a previous commit                      |
| `build`    | Build system changes                           |
| `ci`       | CI/CD workflow changes                         |
| `chore`    | Maintenance, tooling                           |
| `test`     | Adding or fixing tests                         |
| `style`    | Whitespace/formatting only                     |

> **Changelog grouping:** release-please groups commits into changelog sections by type.
> This repo's `.release-please-config.json` recognizes `feat`, `fix`, `perf`, `deps`,
> `docs`, `refactor`, `revert`, and `security`. Note that the PR-title check and
> release-please accept overlapping but not identical sets — `security` and `deps` are
> valid here, but if a PR-title check rejects them, fall back to `fix:` for security
> fixes and `build:`/`chore:` for dependency updates.

Breaking changes: append `!` to the type (`feat!:`) **or** add a `BREAKING CHANGE:`
footer in the commit body. Keep doing this — the footer is what warns an operator,
and CI's `Breaking-change footers survive the squash` check enforces exactly one
declaration per merged commit.

**A breaking change defaults to a MAJOR bump, and usually should not take one.**
This is an application: nothing imports it, so there is no Go `/vN` import-path
requirement and the major number is pure signalling. A release carrying one
behaviour change does not warrant announcing a redesign — that is what took this
repository from `2.7.1` to `3.0.0` in a single afternoon (#345).

So when a PR carries a breaking change, add a `Release-As:` footer naming the next
MINOR, and release-please cuts that instead of the major:

```text
feat(tenancy)!: organization_id is NOT NULL on the partition roots

BREAKING CHANGE: names that were globally unique are now unique per
organization.

Release-As: 3.14.0
```

Two footers, doing two different jobs: `BREAKING CHANGE` tells the operator, and
`Release-As` decides the number. Neither substitutes for the other, and dropping
the first to avoid the second is how a break ships unannounced.

Read the current version off the latest tag (`gh release list --limit 1`) and add
one to the minor. **A genuine major redesign takes the major** — just leave
`Release-As` off and let the default happen.

### `Release-As` must survive the squash as a TRAILER

This is the part that bites, and it has already cost one wrong version here.

`Release-As` only counts when it is in the commit's **trailer block** — the
footers at the very end. This repository squashes with `COMMIT_MESSAGES`, so the
merged commit is every commit body on the branch concatenated in order. A second
commit therefore lands *after* your footer and pushes it out of trailer position:

```text
Release-As: 3.14.0        <- was a trailer on the branch...

Closes #439

* test(repositories): cover the new branches    <- ...and is not one any more
```

release-please then ignores it and cuts the MAJOR that `BREAKING CHANGE` implies.
That is exactly what happened on #499, which asked for `3.14.0` and produced a
release PR for `4.0.0`.

**So on a branch declaring a breaking change, keep the `Release-As` footer in the
LAST commit.** If you add a fix-up commit afterwards — a coverage top-up, a lint
fix, a review response — either amend it into the existing commit, or repeat the
footer in the new one.

If a wrong version has already been computed, do not rewrite `main`. Land a
follow-up commit whose own trailer is the `Release-As` you wanted; release-please
recomputes the pending release PR from the whole range.

Keep the subject line under **72 characters**. Reference issues in the commit body with
`Closes #123`.

---

## Backend (Go) Standards

### Formatting and Vetting

Every commit must pass:

```bash
cd backend
go fmt ./...
go vet ./...
```

Neither command should produce any output (warnings = failure). CI also runs
`go mod tidy` and fails if `go.mod`/`go.sum` are out of date — run `make tidy` and commit
the result.

### Code Comments

Comments are part of the code and are held to the same quality standard:

- **Package-level doc comments** are required for every new package.
- **Exported symbols** must have doc comments.
- **Comments must explain WHY, not just WHAT.** The `statesource` package is a good
  reference — its comments document the *reasons* behind fail-closed write guards and the
  `ErrNotFound` vs transient-error distinction.

### Swagger Annotations

Every new or modified HTTP handler **must** have a complete Swagger annotation block.
`docs/swagger.json` is generated from these annotations and is the OpenAPI source of
truth; CI fails if the committed file is stale.

```go
// @Summary      Short one-line summary
// @Description  Longer description of what this endpoint does.
// @Tags         TagName
// @Security     Bearer
// @Accept       json
// @Produce      json
// @Param        id    path    string  true  "Resource ID (UUID)"
// @Success      200  {object}  SomeResponseType
// @Failure      400  {object}  map[string]interface{}  "Bad request"
// @Failure      401  {object}  map[string]interface{}  "Unauthorized"
// @Failure      500  {object}  map[string]interface{}  "Internal server error"
// @Router       /api/v1/resource/{id} [get]
func (h *Handler) MethodName(c *gin.Context) {
```

After adding or updating annotations, regenerate the spec and commit it:

```bash
make swag   # runs swag init -g cmd/server/main.go --outputTypes json
```

**Annotation rules:**

- Use `// @Security     Bearer` for authenticated endpoints.
- Use `{param}` in `@Router` paths (swag style), **not** `:param` (Gin style).

### Architecture Conventions

- **Repository pattern**: database access goes through `backend/internal/db/repositories/`.
  Handlers must never query the database directly.
- **State sources**: every backend implements the `statesource.Connector` interface in
  `backend/internal/statesource/statesource.go` and is wired into the `statesource.New`
  type switch.
- **Fail-closed writes**: state writes must be refused unless the target's existence can
  be positively verified. Use `statesource.IsNotFound` to distinguish a genuinely missing
  state (safe first write) from a transient backend failure (abort) — never skip the
  pre-write backup and serial/lineage checks on an ambiguous read error.
- **Error handling**: return meaningful errors; do not swallow them. Use
  `fmt.Errorf("context: %w", err)` to preserve the error chain.
- **Context propagation**: pass `context.Context` through all I/O calls; respect
  cancellation.
- **Credentials**: never log decrypted credentials and never return them from the API.
  All source/CI credentials are encrypted with AES-256-GCM (`internal/crypto`).

---

## Database Migrations

Migrations live in `backend/internal/db/migrations/` and are embedded into the binary;
they run on boot under an advisory lock and are also available via the `migrate`
subcommand.

- **Never edit existing migration files.** The migration system treats file content as
  immutable.
- Create a new numbered pair using the next sequential number after the highest existing
  migration (currently `000039`, so the next is `000040`): `0000NN_description.up.sql`
  and `0000NN_description.down.sql`.
- The down migration must fully reverse the up migration.
- Test both directions before submitting.

---

## Adding a New State Source

State sources are the connectors to the backends where Terraform state lives (S3, Azure
Blob, GCS, Consul, PostgreSQL, Kubernetes, HTTP, Git, HCP/TFC, local).

1. Create a new file under `backend/internal/statesource/<name>.go`.
2. Implement the `statesource.Connector` interface (`List`, `Read`, `Write`). If the
   backend supports advisory locking, also implement the `statesource.Locker` interface.
3. Add a `case "<name>":` to the type switch in `statesource.New`
   (`backend/internal/statesource/statesource.go`).
4. Honor the fail-closed contract: return `ErrNotFound` (or wrap `fs.ErrNotExist`) only
   when the state genuinely does not exist; surface transient failures as ordinary errors.
5. Add unit tests covering `List`, `Read`, and `Write` (and `Lock`/`Unlock` if
   implemented). Connectors that reach a live external service should gate those paths
   behind a `coverage:skip:` marker and test the pure logic directly — see the existing
   `*_test.go` files in the package.

---

## Testing Requirements

Before submitting a pull request, run the full local quality gate. CI will reject
anything that does not pass these:

```bash
cd backend

# 1. Format and vet
go fmt ./...
go vet ./...

# 2. Tests with the race detector and coverage (filtered floor: 79%)
go test ./internal/... -race -coverprofile=coverage.out -covermode=atomic
go run ./scripts/coverfilter -in coverage.out -out coverage.filtered.out -root .
go tool cover -func=coverage.filtered.out | grep "^total:"

# 3. Security scan — compare against the committed baseline
gosec -fmt json -out gosec-results.json ./... || true
python3 scripts/gosec-compare.py --results gosec-results.json --baseline gosec-baseline.json --base-dir .
```

If `gosec-compare.py` reports new findings, either fix them, suppress an accepted risk
inline with `// #nosec <rule> -- <reason>`, or (after review) regenerate the baseline:

```bash
bash backend/scripts/update-gosec-baseline.sh
```

### Coverage floor and skip annotations

CI enforces a **79%** filtered-coverage floor (`backend/.github`/`ci.yml`); the floor is
ratcheted up over time, so every change that touches a package should add tests rather
than lower coverage. When a function genuinely cannot be exercised in unit tests (live
DB, OIDC issuer, process-exit paths), mark it with a doc-comment marker:

```go
// coverage:skip:{REASON}
```

The coverage filter (`scripts/coverfilter`) excludes these from the threshold
calculation. Use this sparingly — it is for code that is truly untestable, not code that
is merely inconvenient to test.

Security-sensitive code (authentication, credential encryption, state-write guards, lock
handling) requires tests by default — PRs adding such code without tests will not be
merged.

---

## Pull Request Process

1. **Open an issue first** for substantial changes.
2. **Branch from `main`** and target `main` with your PR.
3. **Use a Conventional Commit PR title** (enforced by CI). Examples:
   - `feat(statesource): add OCI object storage connector`
   - `fix(drift): collapse re-detections into the live record`
   - `docs: update deployment guide for AKS`
4. Write a clear PR description: what changed, why, how you tested it, and a link to the
   issue. Fill in the PR template's **Changelog** entry.
5. All required CI checks must pass: `Backend Tests & Quality`, `Security Scan (gosec)`,
   `Docker Build Smoke Test`, `Deployment artifacts (helm / kustomize / compose)`,
   `Conventional PR Title`, and `Dependency review`.
6. At least one code-owner approval is required before merging.
7. **Squash merge** into `main` — the PR title becomes the commit message.
8. The PR author is responsible for resolving merge conflicts.

---

## Reporting Security Vulnerabilities

**Do not open a public GitHub issue for security vulnerabilities.**

Use [GitHub's private security advisory feature](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability)
to report issues privately, or email the maintainers listed in `CODEOWNERS`. Include a
clear description, steps to reproduce, the potential impact, and any suggested
mitigations. See [SECURITY.md](SECURITY.md) for the full policy and response timeline.

---

## Documentation

Documentation is a first-class deliverable:

- **New features**: update the relevant section of `README.md` and any applicable `docs/`
  files.
- **Configuration changes**: update [docs/configuration.md](docs/configuration.md) with
  the new `TSM_*` variable(s), type, default, and description.
- **New deployment options**: add a section to [docs/deployment.md](docs/deployment.md).
- **API changes**: update the Swagger annotation on the handler and regenerate
  `backend/docs/swagger.json` with `make swag`.
- **Secrets / recovery changes**: update [docs/secrets-rotation.md](docs/secrets-rotation.md)
  and [docs/disaster-recovery.md](docs/disaster-recovery.md) if key handling changes.

PRs that introduce user-visible features without corresponding documentation updates will
be asked to add documentation before merge.

## Tenancy model (estate-wide)

The suite is moving to an explicit tenancy model: **the host is the content tenant**
(modules, providers, binaries belong to a host), **the organisation is the editorial
scope** (who may edit, set policy, approve a version), and the state manager is
**single-host by design**.

**Read [`docs/tenancy-model.md` in terraform-suite-identity](https://github.com/sethbacon/terraform-suite-identity/blob/main/docs/tenancy-model.md) before changing
anything that touches `organization_id`, namespace ownership, the Terraform protocol
surface, or a scoped read.** It also records what must not be done — two of those are
one-way doors that read as ordinary tidy-up.

Most relevant here: **an unscoped read is not automatically a finding.** The registry's
consumption surface is unscoped by design under the current model. A guard should assert
that every unscoped read is *declared*, not that none exists.
