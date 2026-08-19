<!-- markdownlint-disable MD013 -->
# Security Policy

Terraform State Manager (TSM) connects to Terraform state where it already lives and
stores encrypted source and CI credentials. A compromise of this service can expose
infrastructure state and the secrets used to reach it, so we take vulnerability reports
seriously and ask that they be handled with care.

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 1.x     | :white_check_mark: |
| < 1.0   | :x:                |

Releases up to `v0.9.0` were cut from the legacy `archive/ogtsm` lineage and are no
longer supported.

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Instead, please report them privately using one of these methods:

1. **GitHub Security Advisories** — Use the "Report a vulnerability" button on the
   [Security tab](../../security/advisories) of this repository.
2. **Email** — Send details to the repository maintainers listed in `CODEOWNERS`.

### What to Include

- Description of the vulnerability
- Steps to reproduce (proof of concept if possible)
- Affected versions
- Potential impact (especially anything touching stored credentials, the
  `TSM_ENCRYPTION_KEY`, state read/write paths, or authentication)

### Response Timeline

- **Acknowledgement:** within 48 hours
- **Initial assessment:** within 5 business days
- **Fix or mitigation:** targeting 30 days for critical/high severity

### Disclosure Policy

We follow [coordinated disclosure](https://en.wikipedia.org/wiki/Coordinated_vulnerability_disclosure).
We will credit reporters in the release notes unless anonymity is requested.

## Security Practices

- Source and CI credentials are encrypted at rest with **AES-256-GCM**
  (`backend/internal/crypto/`) and are never returned by the API
- Cookie-based JWT sessions are `HttpOnly` and CSRF-protected — tokens are never
  exposed to page JavaScript
- API keys (`tsm_` Bearer tokens) are bcrypt-hashed, shown once, and capped at the
  creator's own scopes
- All releases include SHA-256 checksums and SLSA build-provenance attestations
- Container images and checksum files are signed with [cosign](https://github.com/sigstore/cosign)
  (keyless, Sigstore)
- A software bill of materials (SBOM) is generated for release archives via syft
- Static analysis via `gosec` runs on every PR with baseline drift detection
  (`backend/scripts/gosec-compare.py`); Trivy scans **fail** CI and the release on
  HIGH/CRITICAL vulnerabilities that have an available fix (`exit-code: 1`,
  `ignore-unfixed: true`) — a filesystem scan of `backend/` pre-merge and a scan
  of the published image digest at release
- Dependency review runs on every PR and fails on high-severity advisories
- The application follows common OWASP mitigations: parameterised queries, input
  validation, CSRF protection, security headers, and audit logging

## Known Limitations

- **Pre-write state backups (`state_backups.data`) are not encrypted at rest.**
  Every edit, `rm`/`mv`, or restore takes a full backup of the prior state so
  the change is one-click reversible (see
  [docs/architecture.md](docs/architecture.md#state-edit-pipeline)). Transfer
  is the exception: it always overwrites the target with no backup of the
  target's prior content, and only creates a backup — of the **source**, not
  the target — when a migrate runs with `decommission` and the post-write
  verification succeeds. Unlike source/CI credentials, notification target
  URLs, and API keys, these backups are stored as plaintext and rely only on
  database access control and the `state:read`-scoped API — any secret that
  can appear in live state can appear in a backup row.
- **Backups have no automatic retention or pruning.** The table grows with
  every edit/restore, plus a migrate transfer that decommissions its source —
  the only transfer path that writes a backup row; a plain backup or a
  non-decommissioning migrate adds none. It is only cleared by purging a
  state object outright (`purge=true` on delete); there is no size- or
  age-based pruning today (see
  [docs/capacity-planning.md](docs/capacity-planning.md#database-sizing)).
- Encryption at rest and a retention policy for state backups are planned and
  tracked in #257.
- **`github.com/lib/pq` still appears in the module graph, and its advisories
  will still be reported by a scanner that reads `go.mod` alone.** Nothing in
  this module imports it: the Postgres driver is `jackc/pgx`. It survives as an
  indirect requirement of `golang-migrate/migrate/v4`, whose `database/postgres`
  driver this module deliberately keeps, because the `pgx/v5` equivalent offers
  only `WithInstance` and closes the pool it is handed. Reachability-aware tools
  agree there is no exposure — `govulncheck` reports zero findings, where the
  seven `lib/pq` advisories were previously reported here — but a naive
  composition scan cannot see that and will keep flagging them.

## Encryption Key Custody

`TSM_ENCRYPTION_KEY` protects all stored source and CI credentials and has **no
re-encryption tooling**. If it is lost, encrypted credentials cannot be recovered;
if it leaks, stored credentials must be considered compromised and rotated at their
origin. Escrow this key out of band and rotate deployed credentials if it is exposed.
See [docs/disaster-recovery.md](docs/disaster-recovery.md) and
[docs/secrets-rotation.md](docs/secrets-rotation.md).

## Repository Hardening

The following GitHub repository controls are configured for `main` to protect the
release pipeline and supply chain. The repository is **main-only**: feature and fix
branches are opened directly against `main`, and there is no long-lived development
branch.

### Branch Protection (`main`)

- Required status checks (strict — branch must be up-to-date): `Backend Tests & Quality`,
  `Security Scan (gosec)`, `Docker Build Smoke Test`,
  `Deployment artifacts (helm / kustomize / compose)`, `Conventional PR Title`,
  `Dependency review`
- Required pull request reviews: 1 approving review, dismiss stale reviews, require
  code-owner review
- Required conversation resolution: yes
- Force pushes: blocked; branch deletion: blocked
- The release GitHub App token (`RELEASE_DISPATCH_APP_ID` / `RELEASE_DISPATCH_APP_KEY`)
  is allowed to push release commits and `v*.*.*` tags so that release-please can run

### Merge Strategy

- **Squash merge only** — the PR title becomes the commit message
- Delete branch on merge: enabled

### Supply-Chain Security

- All GitHub Actions are pinned to full commit SHAs
- Secret scanning + push protection: enabled
- `gosec` security scanning in CI with baseline drift detection; a scheduled
  duplicate-suppressed GitHub issue is opened automatically when new findings appear
- `go vet`, `go build`, and race-detector-enabled tests run in CI
- **SLSA provenance attestation** on Docker images and GoReleaser binaries via
  `actions/attest-build-provenance`
- **SBOM generation** via syft in GoReleaser
- **Cosign keyless signing** on Docker images and checksum files via Sigstore
  (verify with `cosign verify` — see [RELEASING.md](RELEASING.md))
