#!/usr/bin/env bash
# Assert per-package coverage minimums for security-critical packages.
#
# ci.yml's main gate enforces a single blended repo-wide coverage floor, which a
# well-covered CRUD package can keep comfortably green even while a sensitive
# package quietly regresses. This check adds named per-package floors so a
# regression concentrated in one security-critical package fails the build on its
# own (#269). Each package is tested in isolation (not with its sub-packages) so
# a low-coverage sub-package can neither dilute nor be diluted by its parent.
set -euo pipefail

# "package|minimum" pairs. Minimums are set a few points below each package's
# actual coverage at the time this gate was added, so it catches regressions
# without blocking on aspirational targets. Ratchet upward as coverage improves.
PACKAGES=(
  "github.com/terraform-state-manager/terraform-state-manager/internal/auth|80"
  "github.com/terraform-state-manager/terraform-state-manager/internal/auth/saml|58"
  "github.com/terraform-state-manager/terraform-state-manager/internal/auth/ldap|35"
  "github.com/terraform-state-manager/terraform-state-manager/internal/middleware|85"
  "github.com/terraform-state-manager/terraform-state-manager/internal/statesource|72"
  "github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories|77"
)

fail=0
for entry in "${PACKAGES[@]}"; do
  pkg="${entry%%|*}"
  min="${entry##*|}"
  # Test the exact package only (not sub-packages) and discard test output.
  go test -coverprofile=/tmp/pkg-coverage.out "${pkg}" >/dev/null 2>&1 || true
  coverage=$(go tool cover -func=/tmp/pkg-coverage.out | grep "^total:" | awk '{print $3}' | tr -d '%')
  if awk -v cov="${coverage}" -v thr="${min}" 'BEGIN { exit !(cov + 0 < thr + 0) }'; then
    echo "FAIL: ${pkg} coverage ${coverage}% is below minimum ${min}%"
    fail=1
  else
    echo "PASS: ${pkg} coverage ${coverage}% >= ${min}%"
  fi
done

if [ "${fail}" -ne 0 ]; then
  echo "One or more package coverage floors were breached — add tests or justify a floor change."
  exit 1
fi
echo "All package coverage checks passed"
