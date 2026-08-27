#!/usr/bin/env bash
# Assert that the -tags integration suites ACTUALLY EXECUTED, per package.
#
# A `go test` run that could not build a package and one whose tests all passed
# do not look alike in the transcript, but they used to look alike to this
# check: it grepped for `--- PASS: .*TestIntegration`, a marker SIX packages
# emit. internal/approles stopped compiling under the tag and the check stayed
# green on its siblings' output for two days (#521). A guard whose evidence can
# be produced by something other than the thing it guards is blind, not clean.
#
# So the expected markers are DERIVED, per package, from the committed source:
# for every integration-tagged file, the test functions declared IN THAT FILE,
# at least one of which must appear as a top-level PASS. Deriving rather than
# listing is deliberate — a transcribed inventory goes stale silently, and a
# stale inventory is how this check went blind in the first place. It also means
# a package whose tagged file builds but whose tagged tests all SKIP fails here,
# because only the untagged tests would have reported.
#
# Usage: assert_integration_tests_ran.sh <go test -v transcript> [source root]
set -euo pipefail

log=${1:?usage: assert_integration_tests_ran.sh <transcript> [source root]}
root=${2:-./internal}

if [[ ! -s "$log" ]]; then
  echo "::error::transcript ${log} is missing or empty -- the test step produced nothing to grade."
  exit 1
fi

# A skip is not a pass. Checked first so an unset or unreachable DSN is reported
# as itself rather than as a missing marker.
dsn_skip='(TSM_)?TEST_DATABASE_URL (not set|is not reachable|unreachable)|not reachable at (TSM_)?TEST_DATABASE_URL'
if grep -qE "$dsn_skip" "$log"; then
  echo "::error::Postgres tests SKIPPED on an unset or unreachable DSN -- this job proved nothing."
  grep -nE "$dsn_skip" "$log" | head -5
  exit 1
fi

# Both build-constraint spellings, and `integration` as a whole term so that
# `//go:build integration && slow` counts while a hypothetical `nointegration`
# does not.
mapfile -t tagged < <(
  grep -rlE '^//[[:space:]]*(go:build|\+build)[[:space:]].*\bintegration\b' \
    --include='*_test.go' "$root" | sort
)

# FLOOR. An empty universe is the exact shape of the bug this file exists to
# stop: nothing to check reads identically to everything checked out fine.
if (( ${#tagged[@]} == 0 )); then
  echo "::error::found NO integration-tagged test files under ${root}."
  echo "::error::Either the tag was renamed or this enumeration broke; either way this check would pass vacuously."
  exit 1
fi

status=0
checked=0
declare -A seen_pkg=()

for file in "${tagged[@]}"; do
  pkg=$(dirname "$file")
  # Top-level test functions declared in THIS file. `^func` excludes methods,
  # and the trailing paren stops `TestFooHelper` from being read as `TestFoo`.
  mapfile -t names < <(
    grep -oE '^func[[:space:]]+Test[A-Za-z0-9_]*[[:space:]]*\(' "$file" |
      sed -E 's/^func[[:space:]]+//; s/[[:space:]]*\($//' |
      grep -vx 'TestMain' | sort -u
  )
  if (( ${#names[@]} == 0 )); then
    echo "::error::${file} carries the integration tag but declares no test functions -- this check cannot key on it."
    status=1
    continue
  fi
  # One marker per PACKAGE: a package's tagged files share a test binary, so any
  # of their tests passing proves the whole package built and ran.
  if [[ -n "${seen_pkg[$pkg]+x}" ]]; then
    seen_pkg[$pkg]+=$'\n'"${names[*]}"
    continue
  fi
  seen_pkg[$pkg]="${names[*]}"
done

for pkg in $(printf '%s\n' "${!seen_pkg[@]}" | sort); do
  found=""
  for name in ${seen_pkg[$pkg]}; do
    # Anchored, and matching the top-level form only: a subtest prints indented
    # as `    --- PASS: Parent/child`, which must not stand in for its parent.
    if grep -qE "^--- PASS: ${name} \(" "$log"; then
      found="$name"
      break
    fi
  done
  if [[ -z "$found" ]]; then
    echo "::error::${pkg}: no integration-tagged test reported a top-level PASS -- that package did not build or did not run."
    printf '::error::  expected a top-level PASS from any one of: %s\n' "$(tr '\n' ' ' <<< "${seen_pkg[$pkg]}")"
    status=1
  else
    echo "ok  ${pkg}  (integration marker: ${found})"
  fi
  checked=$(( checked + 1 ))
done

# The one DSN-gated suite that carries no build tag, so the derivation above
# cannot see it. Named explicitly, and PRECISELY: its sibling
# TestMigrationHelpers_DoNotUseWithInstance is a source-scanning guard needing no
# database, so it passes with the DSN unset and would make this check vacuous for
# the file it is meant to cover. It is the only live proof of #342, whose fix was
# closed on a test that had never executed.
untagged_marker=TestGetMigrationVersion_ReturnsItsConnectionToThePool
if ! grep -rqE "^func[[:space:]]+${untagged_marker}[[:space:]]*\(" --include='*_test.go' "$root"; then
  echo "::error::${untagged_marker} no longer exists in ${root} -- this check is asserting on a test that was renamed or removed."
  exit 1
fi
if ! grep -qE "^--- PASS: ${untagged_marker} \(" "$log"; then
  echo "::error::${untagged_marker} did not pass -- the DATABASE half of migration_conn_leak_test.go did not run."
  status=1
else
  echo "ok  ${root}/db  (untagged DSN-gated marker: ${untagged_marker})"
fi
checked=$(( checked + 1 ))

if (( status != 0 )); then
  exit 1
fi

echo "Integration suites executed in ${checked} package(s); $(grep -c '^--- PASS' "$log") tests passed in total."
