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
#   3. THE TOOLCHAIN, asked at FILE granularity. Both derivations above read the
#      source and match TEXT against the constraint, so a constraint can gain a
#      second, never-set term instead of losing the word:
#
#          //go:build integration    ->    //go:build integration && postgres
#
#      Nothing in this repository sets `postgres`, so the file leaves the build
#      under `-tags integration`. Derivation A still matches `integration` as a
#      whole word; derivation B still reads the unchanged filename. The two
#      agree file-for-file, the package still supplies its other tagged files,
#      the manifest is still satisfied, and the guard exits 0 with
#      internal/tenancy/isolation_integration_test.go -- the cross-tenant leak
#      proof for #393 -- silently out of the build.
#
#      The loss is FILE-granular and the manifest floor is PACKAGE-granular, so
#      no amount of manifest care can see it. `go list` can: it EVALUATES the
#      constraint rather than matching text against it, and reports the files it
#      actually puts in each test binary. `&& postgres`, the older
#      `// +build integration,postgres`, `integration && !integration`, and a
#      term under some other name all read alike to it.
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

# ── DERIVATION C -- THE TOOLCHAIN ────────────────────────────────────────────
#
# `go list` answers the only question that matters here: which files does the
# compiler actually put in the test binary? Asked twice -- once with the tag CI
# passes, once with the tags CI passes by default -- it yields both the set of
# files that are gated behind `integration` and the set that compiles at all.
#
# Failing to ask is a FAILURE, never a skip. A guard whose evidence cannot be
# gathered has not found nothing wrong; it has found nothing.
if ! command -v go >/dev/null 2>&1; then
  echo "::error::the Go toolchain is not on PATH, so the compiler could not be asked which files it"
  echo "::error::builds under -tags integration. Without that answer a file dropped from the"
  echo "::error::integration build is invisible here, so this is a failure rather than a skip."
  exit 1
fi

mod_dir=$(go list -m -f '{{.Dir}}' 2>/dev/null || true)
mod_phys=$([[ -n $mod_dir ]] && cd -- "$mod_dir" 2>/dev/null && pwd -P || true)
if [[ -z $mod_phys ]]; then
  echo "::error::could not locate the Go module root from $(pwd) -- \`go list -m\` reported nothing."
  exit 1
fi
# The two derivations above emit paths relative to the working directory and the
# manifest is module-relative, so they only line up when those are the same
# place. That was always true of this script; it is asserted here rather than
# assumed, because a silent mismatch would make every comparison below compare
# two disjoint sets and report them all as broken for the wrong reason.
if [[ $(pwd -P) != "$mod_phys" ]]; then
  echo "::error::run this from the Go module root (${mod_phys}), not $(pwd -P) -- the derived paths"
  echo "::error::and ${manifest} are module-relative and would not line up from anywhere else."
  exit 1
fi

# One entry per test file the named build compiles, module-relative, sorted so
# `comm` can be used against the derivations above.
golist_fmt='{{$d := .Dir}}{{range .TestGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}'

# Writes to $1; remaining arguments are passed to `go list`. Deliberately NOT a
# command substitution: `exit` inside `$(...)` leaves only the subshell, and this
# function has to be able to abort the script.
compiled_test_files() {
  local dest=$1
  shift
  if ! go list "$@" -f "$golist_fmt" "${root%/}/..." > "$work/golist.out" 2> "$work/golist.err"; then
    echo "::error::\`go list $* ${root%/}/...\` failed, so the compiler could not be asked which files"
    echo "::error::it builds. Without that answer a file dropped from a build is invisible here."
    while IFS= read -r line; do
      [[ -n "$line" ]] && echo "::error::    ${line}"
    done < "$work/golist.err"
    exit 1
  fi
  # Prefix-stripped with a bash expansion, not `sed`, so a module path holding a
  # regex metacharacter cannot quietly fail to strip and desynchronise the sets.
  while IFS= read -r p; do
    [[ -n "$p" ]] && printf '%s\n' "${p#"$mod_phys"/}"
  done < "$work/golist.out" | sort -u > "$dest"
}

# Exactly the two builds .github/workflows/ci.yml runs over this root: the
# default one under `go test ./internal/...` and the tagged one under
# `go test ./internal/... -tags integration`.
compiled_test_files "$work/compiled_tagged" -tags integration
compiled_test_files "$work/compiled_default"

if [[ ! -s "$work/compiled_tagged" ]]; then
  echo "::error::\`go list -tags integration ${root%/}/...\` reported NO test files -- the toolchain"
  echo "::error::floor would pass vacuously, which is the exact shape of the bug it exists to stop."
  exit 1
fi

# The integration-gated set, straight from the compiler: in the tagged build and
# not in the default one.
comm -23 "$work/compiled_tagged" "$work/compiled_default" > "$work/gated"

# FLOOR FOUR. The compiler's gated set and the by-name set must agree, FILE for
# FILE. This is the floor the package manifest cannot supply: it fails for ONE
# file even when its package's siblings still report a PASS.
not_built=$(comm -13 "$work/gated" "$work/named")
not_named=$(comm -23 "$work/gated" "$work/named")
if [[ -n "$not_built" || -n "$not_named" ]]; then
  echo "::error::the compiler and the by-name enumeration of the integration suites DISAGREE."
  if [[ -n "$not_built" ]]; then
    echo "::error::  reads as an integration test but is NOT in the build under -tags integration:"
    indent_list "$not_built"
    echo "::error::  Its build constraint no longer selects it -- most often because a second term"
    echo "::error::  was added that nothing in CI sets, as in \`integration && postgres\`. The word"
    echo "::error::  'integration' is still there, so the text-matching floors above see nothing"
    echo "::error::  wrong, and its package keeps passing on its other files. Restore a constraint"
    echo "::error::  that -tags integration satisfies, or rename the file and its tests if the suite"
    echo "::error::  really is meant to stop being an integration suite."
  fi
  if [[ -n "$not_named" ]]; then
    echo "::error::  built only under -tags integration but does not read as an integration test:"
    indent_list "$not_named"
    echo "::error::  Name the file '*integration_test.go' or a test 'TestIntegration...', so the"
    echo "::error::  by-name floor can see it too."
  fi
  exit 1
fi

# FLOOR FIVE. No test file may sit outside BOTH builds. A constraint that no job
# satisfies -- `integration && !integration`, or a term under a name nothing
# sets -- compiles nowhere, and a file that compiles nowhere proves nothing while
# still reading in the diff as a test that exists.
find "$root" -type f -name '*_test.go' \
  -not -path '*/testdata/*' -not -name '_*' -not -name '.*' 2>/dev/null |
  sed 's|^\./||' | sort -u > "$work/on_disk" || true
cat "$work/compiled_tagged" "$work/compiled_default" | sort -u > "$work/compiled_any"
uncompiled=$(comm -23 "$work/on_disk" "$work/compiled_any")
if [[ -n "$uncompiled" ]]; then
  echo "::error::these test files are in NEITHER build this repository runs -- not the default one"
  echo "::error::and not -tags integration -- so nothing they assert is ever executed:"
  indent_list "$uncompiled"
  echo "::error::  A build constraint naming a term no job sets excludes a file everywhere while"
  echo "::error::  leaving it looking present. Give each a constraint one of the two builds"
  echo "::error::  satisfies, or delete it and say so in the commit."
  exit 1
fi

expected_count=$(wc -l < "$work/expected_pkgs" | tr -d "[:space:]")

if [[ $mode == manifest ]]; then
  echo "ok  ${expected_count} integration package(s) match ${manifest}, by build tag and by name;"
  echo "ok  $(wc -l < "$work/gated" | tr -d "[:space:]") file(s) confirmed in the -tags integration build by the compiler."
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
