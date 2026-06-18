<!-- markdownlint-disable MD013 -->
# Releasing

Releases are fully automated via `release-please.yml` and `release.yml`. The repository
is **main-only**: all releases originate from `main`.

## How it works

1. **Developers merge PRs to `main`** with Conventional Commit titles (`feat:`, `fix:`,
   etc.).

2. **release-please maintains an open release PR** titled `chore(main): release X.Y.Z`.
   - It accumulates `CHANGELOG.md` entries from merged commits, grouped by type
     (`feat`, `fix`, `perf`, `deps`, `docs`, `refactor`, `revert`, `security`).
   - `feat:` → minor bump, `fix:` / `perf:` → patch bump, `feat!:` or `BREAKING CHANGE:`
     → major bump.
   - The PR auto-updates as more commits land on `main`. Review it at any time to preview
     what will ship.

3. **When ready to release**, review and squash-merge the release-please PR. That is the
   only required human action.

4. **release-please pushes a `v*.*.*` tag** using a GitHub App token
   (`RELEASE_DISPATCH_APP_ID` / `RELEASE_DISPATCH_APP_KEY`). The App token is required so
   the tag push triggers downstream workflows — the default `GITHUB_TOKEN` cannot, due to
   GitHub's workflow-recursion guard. release-please also creates a **draft** GitHub
   Release with the changelog (`"draft": true` in `.release-please-config.json`).

5. **`release.yml` fires automatically** from the tag push. It:
   - **Guards** that the tagged commit is reachable from `origin/main` (a tag pushed on
     any other branch fails fast before building anything).
   - Runs CI as a gate (`ci.yml` via `workflow_call`).
   - Builds multi-platform Go binaries (linux/darwin/windows, amd64/arm64; no
     windows/arm64) via GoReleaser.
   - Generates an SBOM (syft) and signs the checksum file with cosign (keyless, Sigstore).
   - Packages `deployments/` into a tarball attached to the release.
   - Pushes the Docker image to `ghcr.io/<owner>/terraform-state-manager-backend`
     (tagged with version + `latest`).
   - Attests build provenance for binaries and the container via GitHub Artifact
     Attestations.
   - Signs the container image with cosign (keyless, Sigstore).
   - Uploads assets to the draft GitHub Release and publishes it (which makes it
     immutable).

## Verifying supply-chain attestations

```bash
# Verify binary provenance
gh attestation verify <binary-file> --repo sethbacon/terraform-state-manager-backend

# Verify container provenance
gh attestation verify oci://ghcr.io/sethbacon/terraform-state-manager-backend:vX.Y.Z \
  --repo sethbacon/terraform-state-manager-backend

# Verify cosign signature
cosign verify \
  --certificate-identity-regexp 'https://github\.com/sethbacon/terraform-state-manager-backend/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/sethbacon/terraform-state-manager-backend:vX.Y.Z
```

## Cutting a release

1. Find the open release-please PR (`chore(main): release X.Y.Z`) in the PR list.
2. Review the CHANGELOG entries and version bump — adjust by merging additional `fix:` or
   `feat:` commits if needed.
3. Squash-merge the release PR.
4. Watch `release.yml` run in the Actions tab. No manual dispatch required.

## Hotfix flow

1. Create a `fix/` branch from `main`.
2. Merge the fix PR with a `fix: ...` Conventional Commit title.
3. release-please updates the open release PR with the patch bump.
4. Merge the release PR to ship.

## Manual fallback

If `release-please.yml` fails or the App token is unavailable, the release can be cut by
pushing a tag manually. `release.yml` fires from any `v*.*.*` tag push, provided the tag
is reachable from `main` (the branch guard enforces this).

```bash
git checkout main
git pull

# Edit CHANGELOG.md with the new section, then bump the manifest version.
# .release-please-manifest.json tracks the current version (e.g. {".":"1.6.0"}).

git add CHANGELOG.md .release-please-manifest.json
git commit -m "chore: release vX.Y.Z"
git push origin main

git tag -a vX.Y.Z -m "Release vX.Y.Z"
git push origin vX.Y.Z
```

If no draft release exists for the tag (e.g. a manual `workflow_dispatch`), the
`publish-release` job creates the release from scratch, extracting the matching section
from `CHANGELOG.md`.

## GitHub App key rotation

The release App's private key is stored as `RELEASE_DISPATCH_APP_KEY` in repository
secrets; its Client ID is stored as `RELEASE_DISPATCH_APP_ID` in repository variables.

To rotate the key:

1. Go to GitHub → Settings → Developer settings → GitHub Apps → the release App →
   Private keys.
2. Generate a new key and download it.
3. Update `RELEASE_DISPATCH_APP_KEY` in repository secrets with the new key content.
4. Delete the old private key from the App settings.

## Rollback procedure

To undo a release after the tag has been pushed:

```bash
# Revert the release commit on main
git revert HEAD --no-edit   # or the specific release commit SHA
git push origin main
```

release-please will propose a new release PR on the next `main` push. The already-
published GitHub Release and Docker image can be deleted manually from the GitHub
Releases page and `ghcr.io` if necessary (note: published releases are immutable, so a
new patch release is usually preferable to deletion).

## Deployment config bump (after release)

Deployment artifacts that reference image tags are updated manually after each release.
The image tag follows the published version:

- **Helm chart** (`deployments/helm/`): update `backend.image.tag` in the relevant
  `values.yaml` / `values-aks.yaml` / `values-eks.yaml` / `values-gke.yaml`.
- **Kustomize overlays** (`deployments/kubernetes/overlays/`): update `newTag` for the
  backend image in the affected overlay's `kustomization.yaml`.

CI validates these artifacts on every PR (`helm lint`/`template` + kubeconform, kustomize
builds, and a production `docker compose config` check).
