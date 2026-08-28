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
# listing is deliberate -- a transcribed inventory goes stale silently, and a
# stale inventory is how this check went blind in the first place. It also means
# a package whose tagged file builds but whose tagged tests all SKIP fails here,
# because only the untagged tests would have reported.
#
# # The derivation is itself a thing that can be switched off
#
# Deriving the expected set from the build tag has one hole, and it is the SAME
# hole one level up: change `//go:build integration` to `//go:build
# integration_disabled`, or delete the tagged file, and the package leaves the
# derived set. Nothing is then expected of it, nothing is missing, and the check
# passes -- having silently stopped covering an entire suite. The old floor here
# only caught the case where EVERY file lost the tag. The count was printed and
# never asserted.
#
# Two independent floors close that:
#
#   1. A SECOND DERIVATION that does not read the build tag. A file counts as an
#      integration file if its name says so or if it declares a `TestIntegration`
#      function. The tag-derived set and the name-derived set must be IDENTICAL.
#      Editing the tag moves a file out of the first and leaves it in the second,
#      which is now a hard failure instead of a silent shrink.
#
#   2. A COMMITTED PACKAGE MANIFEST, integration_packages.txt, compared BOTH
#      ways against the derived packages. That survives the evasion the two
#      derivations share: deleting the file, or renaming it and its functions,
#      removes it from both. Dropping a suite is still possible -- it just has to
#      be spelled out in a diff rather than fall out of a build-tag edit.
#
# Usage:
#   assert_integration_tests_ran.sh <go test -v transcript> [source root] [manifest]
#   assert_integration_tests_ran.sh --check-manifest [source root] [manifest]
#
# The second form runs the derivation and manifest floors ALONE, with no
# transcript. It is what the Go-side guard calls, so that manifest drift fails in
# the ordinary unit-test job rather than only in the Postgres job -- and it calls
# this script rather than reimplementing it, because a second copy of a check is
# the defect this file is about.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

mode=grade
log=
if [[ ${1:-} == --check-manifest ]]; then
  mode=manifest
  shift
else
  log=${1:?usage: assert_integration_tests_ran.sh <transcript> [source root] [manifest]}
  shift || true
fi

root=${1:-./internal}
manifest=${2:-${here}/integration_packages.txt}

if [[ $mode == grade ]]; then
  if [[ ! -s "$log" ]]; then
    echo "::error::transcript ${log} is missing or empty -- the test step produced nothing to grade."
    exit 1
  fi

  # A skip is not a pass. Checked first so an unset or unreachable DSN is
  # reported as itself rather than as a missing marker.
  dsn_skip='(TSM_)?TEST_DATABASE_URL (not set|is not reachable|unreachable)|not reachable at (TSM_)?TEST_DATABASE_URL'
  if grep -qE "$dsn_skip" "$log"; then
    echo "::error::Postgres tests SKIPPED on an unset or unreachable DSN -- this job proved nothing."
    # grep -m 5, not `| head -5`: under the pipefail above, head closing the
    # pipe SIGPIPEs grep once its output exceeds the pipe buffer, and the 141
    # aborts before the sample prints -- losing the diagnostic that names which
    # tests skipped, in the run where they did. -m 5 stops grep, so no pipe.
    grep -nE -m 5 "$dsn_skip" "$log"
    exit 1
  fi
fi

if [[ ! -d "$root" ]]; then
  echo "::error::source root ${root} is not a directory -- the enumeration below would find nothing and pass vacuously."
  exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Print a newline-separated list one entry per annotation line. A quoted
# expansion into `printf` would emit the whole list as one argument, and an
# unquoted one would glob; neither is what a reader needs to see.
indent_list() {
  while IFS= read -r entry; do
    [[ -n "$entry" ]] && echo "::error::    ${entry}"
  done <<< "$1"
  return 0
}

# `sed` strips the `./` that grep and find prepend, so the two derivations and
# the manifest are all in one spelling and can be compared with `comm`.

# DERIVATION A -- the build tag. Both constraint spellings, and `integration` as
# a whole term so that `//go:build integration && slow` counts while a
# hypothetical `nointegration` does not.
grep -rlE '^//[[:space:]]*(go:build|\+build)[[:space:]].*\bintegration\b' \
  --include='*_test.go' "$root" 2>/dev/null | sed 's|^\./||' | sort -u > "$work/tagged" || true

# DERIVATION B -- the NAME, which the build-tag edit does not touch. A file is an
# integration file if it is called one -- the convention here is that the name
# ENDS in `integration_test.go` -- or if it declares a `TestIntegration` test.
{
  grep -rlE '^func[[:space:]]+TestIntegration' --include='*_test.go' "$root" 2>/dev/null || true
  find "$root" -type f -name '*integration_test.go' 2>/dev/null || true
} | sed 's|^\./||' | sort -u > "$work/named" || true

mapfile -t tagged < "$work/tagged"

# FLOOR ONE. An empty universe is the exact shape of the bug this file exists to
# stop: nothing to check reads identically to everything checked out fine.
if (( ${#tagged[@]} == 0 )); then
  echo "::error::found NO integration-tagged test files under ${root}."
  echo "::error::Either the tag was renamed or this enumeration broke; either way this check would pass vacuously."
  exit 1
fi

# FLOOR TWO. The two derivations must agree, FILE for FILE.
lost_tag=$(comm -13 "$work/tagged" "$work/named")
lost_name=$(comm -23 "$work/tagged" "$work/named")
if [[ -n "$lost_tag" || -n "$lost_name" ]]; then
  echo "::error::the build-tag and by-name enumerations of the integration suites DISAGREE."
  if [[ -n "$lost_tag" ]]; then
    echo "::error::  reads as an integration test but carries NO integration build tag:"
    indent_list "$lost_tag"
    echo "::error::  A file that loses its tag leaves the expected set, and nothing is then missing"
    echo "::error::  from the transcript. Restore the tag, or rename the file and its tests if the"
    echo "::error::  suite really is meant to stop being an integration suite."
  fi
  if [[ -n "$lost_name" ]]; then
    echo "::error::  carries the integration build tag but does not read as one:"
    indent_list "$lost_name"
    echo "::error::  Name the file '*integration_test.go' or a test 'TestIntegration...', so the"
    echo "::error::  by-name floor can see it if the tag is ever removed."
  fi
  exit 1
fi

printf '%s\n' "${tagged[@]}" | xargs -n1 dirname | sort -u > "$work/derived_pkgs"

# FLOOR THREE. The committed package manifest, compared BOTH ways.
if [[ ! -s "$manifest" ]]; then
  echo "::error::package manifest ${manifest} is missing or empty -- the package floor cannot be applied,"
  echo "::error::and without it a suite that stops running is indistinguishable from one that passes."
  exit 1
fi
# `|| true` because grep exits 1 on a manifest holding only comments, and under
# `set -e` with pipefail that would abort HERE, before the empty-floor check
# below could say why. A guard that dies silently is the shape being fixed.
sed -E 's/#.*$//; s/[[:space:]]+$//; s|^\./||' "$manifest" | grep -vE '^[[:space:]]*$' | sort -u > "$work/expected_pkgs" || true
if [[ ! -s "$work/expected_pkgs" ]]; then
  echo "::error::package manifest ${manifest} lists no packages -- an empty floor is not a floor."
  exit 1
fi

missing_pkgs=$(comm -23 "$work/expected_pkgs" "$work/derived_pkgs")
extra_pkgs=$(comm -13 "$work/expected_pkgs" "$work/derived_pkgs")
if [[ -n "$missing_pkgs" || -n "$extra_pkgs" ]]; then
  echo "::error::the integration package set does not match ${manifest}."
  if [[ -n "$missing_pkgs" ]]; then
    echo "::error::  EXPECTED but no longer supplying an integration-tagged test:"
    indent_list "$missing_pkgs"
    echo "::error::  This is how a whole suite goes missing without anything looking wrong (#521)."
    echo "::error::  Restore the suite, or delete the line from the manifest and say so in the commit."
  fi
  if [[ -n "$extra_pkgs" ]]; then
    echo "::error::  supplying integration-tagged tests but NOT listed in the manifest:"
    indent_list "$extra_pkgs"
    echo "::error::  Add the package to the manifest so that it, too, is held to the floor."
  fi
  exit 1
fi

expected_count=$(wc -l < "$work/expected_pkgs" | tr -d "[:space:]")

if [[ $mode == manifest ]]; then
  echo "ok  ${expected_count} integration package(s) match ${manifest}, by build tag and by name."
  exit 0
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

# ASSERT the count, do not merely print it. The manifest already pinned WHICH
# packages must report; this pins that the loop above actually walked them, so a
# derivation that collapses between the floors and here cannot look clean.
want_checked=$(( expected_count + 1 ))
if (( checked != want_checked )); then
  echo "::error::graded ${checked} package(s) but the manifest and the untagged marker require ${want_checked}."
  status=1
fi

if (( status != 0 )); then
  exit 1
fi

echo "Integration suites executed in ${checked} package(s), matching ${manifest}; $(grep -c '^--- PASS' "$log") tests passed in total."
