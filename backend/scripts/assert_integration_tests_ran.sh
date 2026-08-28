#!/usr/bin/env bash
# Assert that the -tags integration suites ACTUALLY EXECUTED, per package and
# per file.
#
# A `go test` run that could not build a package and one whose tests all passed
# do not look alike in the transcript, but they used to look alike to this
# check: it grepped for `--- PASS: .*TestIntegration`, a marker SIX packages
# emit. internal/approles stopped compiling under the tag and the check stayed
# green on its siblings' output for two days (#521). A guard whose evidence can
# be produced by something other than the thing it guards is blind, not clean.
#
# So the expected markers are DERIVED, per file, from the committed source: for
# every integration-tagged file, the test functions declared IN THAT FILE, at
# least one of which must appear as a top-level PASS. Deriving rather than
# listing is deliberate -- a transcribed inventory goes stale silently, and a
# stale inventory is how this check went blind in the first place.
#
# # The derivation is itself a thing that can be switched off
#
# Deriving the expected set from the build tag has one hole, and it is the SAME
# hole one level up: change `//go:build integration` to `//go:build
# integration_disabled`, or delete the tagged file, and the package leaves the
# derived set. Nothing is then expected of it, nothing is missing, and the check
# passes -- having silently stopped covering an entire suite.
#
# The floors, in the order they are applied:
#
#   1. A NON-EMPTY UNIVERSE. Finding no integration-tagged files at all is the
#      exact shape of the bug this file exists to stop, so it is a failure.
#
#   2. A SECOND DERIVATION that does not read the build tag. A file counts as an
#      integration file if its name says so or if it declares a `TestIntegration`
#      function. The tag-derived set and the name-derived set must be IDENTICAL.
#      Editing the tag moves a file out of the first and leaves it in the second,
#      which is now a hard failure instead of a silent shrink.
#
#   3. A COMMITTED PACKAGE MANIFEST, integration_packages.txt, compared BOTH
#      ways against the derived packages. That survives the evasion the two
#      derivations share: deleting the file, or renaming it and its functions,
#      removes it from both. Dropping a suite is still possible -- it just has to
#      be spelled out in a diff rather than fall out of a build-tag edit.
#
#   4. THE TOOLCHAIN, asked at FILE granularity. Both derivations above read the
#      source and match TEXT against the constraint, so a constraint can gain a
#      second, never-set term instead of losing the word:
#
#          //go:build integration    ->    //go:build integration && postgres
#
#      Nothing in this repository sets `postgres`, so the file leaves the build
#      under `-tags integration` while both text floors stay satisfied. `go
#      list` EVALUATES the constraint rather than matching text against it, so
#      the compiler's gated set and the by-name set must agree file for file.
#
#   5. NO TEST FILE OUTSIDE BOTH BUILDS. A constraint that no job satisfies --
#      `integration && !integration`, or a term under a name nothing sets --
#      compiles nowhere, and a file that compiles nowhere proves nothing while
#      still reading in the diff as a test that exists.
#
#   6. NO FILE IGNORED UNDER THE TAGGED BUILD, unless a committed allowlist
#      names it WITH A REASON. Floors 1-5 inspect only TEST files, so a
#      never-set second term did not have to live in a test file at all. Move
#      the load-bearing code into a NON-test pair --
#
#          //go:build postgres     (the real implementation)
#          //go:build !postgres    (a no-op stub)
#
#      -- and the test file keeps its tag, its name, its compiled membership and
#      its PASS marker, while the thing it exercises has been swapped for a stub.
#      Every floor above exits 0 on that tree; it was demonstrated against this
#      script before floor 6 existed. The compiler again has the answer:
#      `go list -tags integration` reports, per package, exactly the files it
#      EXCLUDED for build-constraint reasons, as IgnoredGoFiles. Under `-tags
#      integration` this module expects that set to be EMPTY, module-wide: the
#      default files carry no constraint and the integration files carry a
#      constraint the tag satisfies. Any file in it is either the relocation
#      above or a deliberate exception, and a deliberate exception is accounted
#      for in scripts/integration_ignored_files.txt -- per file, with a reason,
#      compared BOTH ways so a stale or pre-planted entry is as loud as a
#      missing one. The same accounted-or-failing contract as the manifest.
#
#      IgnoredGoFiles alone is not the whole floor. `go list ./...` silently
#      SKIPS a directory whose files are ALL constraint-excluded -- it is not
#      reported as a package, not even with -e, so a real implementation parked
#      alone in its own all-excluded directory appears in nobody's
#      IgnoredGoFiles. That evasion was demonstrated against an earlier draft
#      of this floor. So the floor is completed from the filesystem: every .go
#      file on disk, module-wide -- skipping only testdata, vendor and
#      underscore- or dot-prefixed files, which the toolchain refuses to import
#      from anywhere -- must be accounted for by the compiler under `-tags
#      integration` as compiled, ignored or invalid. A file the compiler cannot
#      even be asked about is treated exactly like an ignored one: allowlisted
#      with a reason, or a failure naming it.
#
#      GOOS/GOARCH files land in this floor DELIBERATELY. A file excluded by
#      platform -- `conn_windows.go` by name, or `//go:build windows` by
#      constraint -- is in IgnoredGoFiles too, and it is NOT auto-tolerated,
#      because "compiles on some other platform" is exactly the relocation
#      shape wearing a legitimate name: a `windows` implementation with a
#      `!windows` stub neuters a proof on Linux CI precisely as `postgres` does.
#      A genuine platform-specific file therefore fails ONCE, loudly, and is
#      then admitted by a one-line allowlist entry whose reason a reviewer can
#      weigh. This module has no such files today, so the allowlist is empty.
#
#      The platform of record is CI's: every `go list` here is pinned to
#      GOOS=linux GOARCH=amd64, so a run on a darwin or windows workstation
#      grades the SAME build CI grades instead of its own.
#
#   7. MARKER UNIQUENESS. The grading below keys on test-function names in a
#      transcript that does not say which package printed a line. If a name
#      declared in an integration-gated file were declared ANYWHERE else in the
#      tagged build, a sibling's PASS could stand in for it. So every name this
#      script grades on must be declared exactly once across all test files of
#      the tagged build; a duplicate is a failure naming both declarations.
#
#   8. ONE PASS PER FILE, not per package. A package's tagged files share a
#      test binary, so a sibling file's PASS used to satisfy the whole package
#      while one file's tests were skipped or narrowed away. Now every
#      integration-gated FILE must contribute at least one top-level PASS from
#      the functions it declares, so silencing a whole file -- t.Skip in all of
#      its tests, a `-run` filter that excludes it, or a build edit floor 4
#      catches earlier -- fails naming the file.
#
# # What this guard still cannot see -- the honest residual
#
#   - A GUTTED BODY. A test that keeps its name and prints PASS while asserting
#     nothing is indistinguishable, in a transcript, from the test it used to
#     be. The same applies to a stub swapped into the SAME file as the real
#     implementation with no build constraint at all: that is an ordinary code
#     change, semantics no transcript floor can grade. Only review sees it.
#     Deleting the real implementation outright, or parking it under testdata
#     or an underscore-prefixed name where the toolchain refuses to look, is
#     the same class -- the file is then dead EVERYWHERE, which is a deletion
#     wearing a different diff, and floor six exempts those paths precisely
#     because nothing CI builds can reach them.
#   - A SKIP BESIDE A PASS. If one test in a multi-test file calls t.Skip while
#     a sibling IN THE SAME FILE passes, the file still contributes its PASS
#     and the skip is invisible here. This is left open deliberately: the suite
#     contains a legitimate conditional skip -- approles' topology-dependent
#     case -- and the DSN skips, so refusing every `--- SKIP` would fail honest
#     runs. Only the DSN skips are fatal, via the dedicated check above the
#     floors. A skip that silences an ENTIRE file is caught by floor 8.
#   - `-run` NARROWING WITHIN A FILE. A filter that keeps at least one passing
#     test per file satisfies floor 8. Narrowing that silences a whole file is
#     caught; narrowing inside a file is not.
#   - SUBTESTS. Markers are top-level by design -- an indented `--- PASS:
#     Parent/child` must not stand in for its parent -- so a skipped or gutted
#     t.Run child under a passing parent is invisible.
#   - THE ALLOWLIST IS A DOOR. An entry admits an ignored file on the strength
#     of prose. The bidirectional check stops stale and pre-planted entries; it
#     cannot stop a reviewer from accepting a bad reason. The contract is that
#     the hole must be VISIBLE in a diff, not that it is impossible.
#   - THE TRANSCRIPT IS TRUSTED. Grading believes the file it is handed. A step
#     that substitutes or edits the transcript before this script runs defeats
#     every floor here; that is the workflow's integrity problem, not this
#     script's, and it is not claimed as covered.
#
# Usage:
#   assert_integration_tests_ran.sh <go test -v transcript> [source root] [manifest] [ignored-allowlist]
#   assert_integration_tests_ran.sh --check-manifest [source root] [manifest] [ignored-allowlist]
#
# The second form runs every source-shaped floor -- 1 through 7 -- with no
# transcript. It is what the Go-side guard calls, so that manifest drift, a
# relocated implementation or a duplicated marker fails in the ordinary
# unit-test job rather than only in the Postgres job -- and it calls this script
# rather than reimplementing it, because a second copy of a check is the defect
# this file is about.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

mode=grade
log=
if [[ ${1:-} == --check-manifest ]]; then
  mode=manifest
  shift
else
  log=${1:?usage: assert_integration_tests_ran.sh <transcript> [source root] [manifest] [ignored-allowlist]}
  shift || true
fi

root=${1:-./internal}
manifest=${2:-${here}/integration_packages.txt}
allowlist=${3:-${here}/integration_ignored_files.txt}

# The build this guard grades is the one CI runs: linux/amd64. Pinned so a
# workstation run of --check-manifest (the Go-side guard runs on every dev
# platform) asks about CI's build rather than its own.
ci_goos=linux
ci_goarch=amd64

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
# compiler actually put in the test binary -- and, just as load-bearing, which
# files did it EXCLUDE for build-constraint reasons? Asked with the tag CI
# passes and with the default tag set, it yields the gated set, the compiled
# universe, and the ignored set the floors below stand on.
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
golist_test_fmt='{{$d := .Dir}}{{range .TestGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}'
# And one per file the named build EXCLUDED for build-constraint reasons --
# test and non-test alike, which is the point of floor six.
golist_ignored_fmt='{{$d := .Dir}}{{range .IgnoredGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}'
# And every .go file the compiler can be ASKED about at all in the named build:
# compiled into the package or its tests, excluded by constraint, or present
# but unparseable. A file on disk that is in none of these lists sits in a
# directory `./...` does not even report, which floor six treats as excluded.
golist_accounted_fmt='{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .CgoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .TestGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .IgnoredGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .InvalidGoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}'

# golist_files <dest> <fmt> <pattern> [go list args...]. Deliberately NOT a
# command substitution: `exit` inside `$(...)` leaves only the subshell, and this
# function has to be able to abort the script.
golist_files() {
  local dest=$1 fmt=$2 pattern=$3
  shift 3
  if ! GOOS="$ci_goos" GOARCH="$ci_goarch" go list "$@" -f "$fmt" "$pattern" > "$work/golist.out" 2> "$work/golist.err"; then
    echo "::error::\`go list $* ${pattern}\` failed, so the compiler could not be asked which files"
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
golist_files "$work/compiled_tagged" "$golist_test_fmt" "${root%/}/..." -tags integration
golist_files "$work/compiled_default" "$golist_test_fmt" "${root%/}/..."

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

# FLOOR SIX. Nothing may be EXCLUDED from the tagged build, module-wide, unless
# the committed allowlist accounts for it per file, with a reason. Floors 1-5
# look only at TEST files, so a never-set second term relocated into a NON-test
# stub pair passed all of them while the compiler quietly built the stub. The
# universe here is `./...`, the whole module, not just ${root}: an integration
# test may import code from anywhere in the module, so a pair parked outside
# the graded root would otherwise escape.
#
# Exclusion has TWO shapes, and both are gathered. A constraint-excluded file
# in a directory that still forms a package appears in that package's
# IgnoredGoFiles. But a directory whose files are ALL excluded is silently
# dropped from `./...` -- no package, no IgnoredGoFiles, no error, not even
# with -e -- so the second query walks the FILESYSTEM and demands that every
# .go file on disk is accounted for by the compiler somewhere: compiled,
# ignored or invalid. testdata, vendor and underscore- or dot-prefixed files
# are outside the toolchain's import universe entirely -- nothing CI builds can
# reach them -- so they are the only exemptions.
golist_files "$work/ignored_tagged" "$golist_ignored_fmt" "./..." -tags integration
golist_files "$work/accounted_tagged" "$golist_accounted_fmt" "./..." -tags integration

find . -type f -name '*.go' \
  -not -path './vendor/*' -not -path '*/testdata/*' -not -name '_*' -not -name '.*' 2>/dev/null |
  sed 's|^\./||' | sort -u > "$work/on_disk_go" || true
comm -23 "$work/on_disk_go" "$work/accounted_tagged" > "$work/vanished"
# `grep -v` drops the empty line an empty half contributes; it exits 1 when
# both halves are empty, which is the good state, hence `|| true`.
cat "$work/ignored_tagged" "$work/vanished" | grep -v '^$' | sort -u > "$work/excluded_tagged" || true

: > "$work/allowed"
allow_bad=0
if [[ -f "$allowlist" ]]; then
  # Entry lines are `<module-relative path> # <reason>`. Full-line comments and
  # blanks are ignored. An entry WITHOUT a reason is refused: the reason is the
  # reviewable half of the contract, not decoration.
  while IFS= read -r line; do
    [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
    entry_path=${line%%[[:space:]]*}
    rest=${line#"$entry_path"}
    if [[ "$rest" != *"#"* ]]; then
      echo "::error::allowlist entry in ${allowlist} carries no reason: ${line}"
      echo "::error::  Every admitted ignored file must say WHY it is legitimately out of the"
      echo "::error::  -tags integration build, as '<path>  # <reason>'."
      allow_bad=1
      continue
    fi
    reason=${rest#*#}
    if [[ -z "${reason//[[:space:]]/}" ]]; then
      echo "::error::allowlist entry in ${allowlist} carries no reason: ${line}"
      echo "::error::  A bare '#' is not a reason."
      allow_bad=1
      continue
    fi
    printf '%s\n' "${entry_path#./}" >> "$work/allowed"
  done < "$allowlist"
fi
if (( allow_bad != 0 )); then
  exit 1
fi
sort -u "$work/allowed" -o "$work/allowed"
# NOTE: an empty or absent allowlist is the GOOD state, not a vacuous floor --
# the assertion is that the ignored set is empty, and the allowlist only admits
# named exceptions to it. This is unlike the package manifest, whose emptiness
# would remove a floor.

unaccounted=$(comm -23 "$work/excluded_tagged" "$work/allowed")
stale_allow=$(comm -13 "$work/excluded_tagged" "$work/allowed")
if [[ -n "$unaccounted" || -n "$stale_allow" ]]; then
  if [[ -n "$unaccounted" ]]; then
    echo "::error::these files are EXCLUDED from the build under -tags integration -- the very build"
    echo "::error::this job runs -- and the allowlist ${allowlist} does not account for them:"
    while IFS= read -r f; do
      [[ -z "$f" ]] && continue
      constraint=$(grep -m1 -E '^//[[:space:]]*(go:build|\+build)' "$f" 2>/dev/null || true)
      if grep -qxF "$f" "$work/vanished"; then
        echo "::error::    ${f}    [in a directory the compiler reports NO package for -- every file there is excluded${constraint:+; }${constraint}]"
      elif [[ -n "$constraint" ]]; then
        echo "::error::    ${f}    [${constraint}]"
      else
        echo "::error::    ${f}    [excluded by its GOOS/GOARCH file name]"
      fi
    done <<< "$unaccounted"
    echo "::error::  This is the relocation axis: a term nothing sets does not have to live in a"
    echo "::error::  test file. Behind '//go:build postgres' with a '!postgres' stub beside it, the"
    echo "::error::  REAL implementation leaves the build while the test keeps its tag, its name and"
    echo "::error::  its PASS -- against the stub. Platform-gated files land here too, deliberately:"
    echo "::error::  a 'windows' file with a '!windows' stub is the same shape wearing a GOOS name."
    echo "::error::  Give the file a constraint the -tags integration build satisfies, delete it, or"
    echo "::error::  add it to ${allowlist} with a reason a reviewer can weigh."
  fi
  if [[ -n "$stale_allow" ]]; then
    echo "::error::these allowlist entries in ${allowlist} name files that are NOT excluded from"
    echo "::error::the -tags integration build (or no longer exist):"
    indent_list "$stale_allow"
    echo "::error::  A stale entry is a door left propped open: it would silently admit a future"
    echo "::error::  ignored file at that path without a fresh review. Remove the entry, or fix the"
    echo "::error::  path if it was mistyped."
  fi
  exit 1
fi

# ── PER-FILE MARKERS ─────────────────────────────────────────────────────────
#
# The declared top-level test functions of every test file in the tagged build,
# as `<file>TAB<name>`, straight from the compiler-confirmed file list --
# grading keys derived from anything less were the round-1 defect. `^func`
# excludes methods, and the trailing paren stops `TestFooHelper` from being
# read as `TestFoo`.
tr '\n' '\0' < "$work/compiled_tagged" |
  xargs -0 grep -oHE '^func[[:space:]]+Test[A-Za-z0-9_]*[[:space:]]*\(' -- 2>/dev/null |
  awk -F: '{
    file = $1
    name = $2
    sub(/^func[ \t]+/, "", name)
    sub(/[ \t]*\($/, "", name)
    if (name != "TestMain") print file "\t" name
  }' | sort -u > "$work/decls" || true

# Every gated file must DECLARE something to key on; a tagged file holding only
# helpers cannot be graded and is refused rather than silently passed over.
cut -f1 "$work/decls" | sort -u > "$work/decl_files"
undeclared=$(comm -23 "$work/gated" "$work/decl_files")
if [[ -n "$undeclared" ]]; then
  echo "::error::these files carry the integration tag but declare no test functions -- this check"
  echo "::error::cannot key on them:"
  indent_list "$undeclared"
  exit 1
fi

# FLOOR SEVEN. Every name graded below must be unique across the ENTIRE tagged
# build. The transcript does not say which package printed a PASS line, so a
# duplicate name anywhere in the same `go test` run could stand in for the
# marker -- the alias would swap silently, exactly the failure mode this script
# exists to stop. Names are cheap; rename the newcomer.
collisions=$(awk -F'\t' '
  NR == FNR { gated[$1] = 1; next }
  {
    count[$2]++
    where[$2] = where[$2] ", " $1
    if (gated[$1]) marker[$2] = 1
  }
  END {
    for (n in marker) if (count[n] > 1) print n " -- declared in: " substr(where[n], 3)
  }' "$work/gated" "$work/decls" | sort)
if [[ -n "$collisions" ]]; then
  echo "::error::these integration marker names are declared more than once in the -tags integration"
  echo "::error::build, so a PASS line in the transcript cannot be attributed to the file it is"
  echo "::error::supposed to prove:"
  indent_list "$collisions"
  echo "::error::  Rename one of the duplicates; the grading below keys on names being unique."
  exit 1
fi

expected_count=$(wc -l < "$work/expected_pkgs" | tr -d "[:space:]")
gated_count=$(wc -l < "$work/gated" | tr -d "[:space:]")

if [[ $mode == manifest ]]; then
  echo "ok  ${expected_count} integration package(s) match ${manifest}, by build tag and by name;"
  echo "ok  ${gated_count} file(s) confirmed in the -tags integration build by the compiler;"
  echo "ok  $(wc -l < "$work/excluded_tagged" | tr -d "[:space:]") file(s) excluded from the -tags integration build, all accounted for in ${allowlist}."
  exit 0
fi

# FLOOR EIGHT -- the grading. One PASS per FILE, keyed on the compiler-confirmed
# gated set. A package's files share a test binary, so under the old per-package
# marker a sibling file's PASS covered a file whose tests were skipped or
# narrowed away; the reported marker silently swapped. Now each file must prove
# itself with a top-level PASS from a function IT declares.
status=0
files_graded=0
declare -A pkgs_graded=()

while IFS= read -r file; do
  [[ -z "$file" ]] && continue
  mapfile -t names < <(awk -F'\t' -v f="$file" '$1 == f { print $2 }' "$work/decls")
  found=""
  for name in "${names[@]}"; do
    # Anchored, and matching the top-level form only: a subtest prints indented
    # as `    --- PASS: Parent/child`, which must not stand in for its parent.
    if grep -qE "^--- PASS: ${name} \(" "$log"; then
      found="$name"
      break
    fi
  done
  if [[ -z "$found" ]]; then
    echo "::error::${file}: none of its test functions reported a top-level PASS -- that file did not build or did not run."
    echo "::error::  A sibling file's PASS no longer covers it: every integration-gated file must"
    echo "::error::  prove itself. If its tests were skipped or filtered, this is that, made loud."
    printf '::error::  expected a top-level PASS from any one of: %s\n' "${names[*]}"
    status=1
  else
    echo "ok  ${file}  (integration marker: ${found})"
  fi
  pkgs_graded[$(dirname "$file")]=1
  files_graded=$(( files_graded + 1 ))
done < "$work/gated"

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

# ASSERT the counts, do not merely print them. The manifest already pinned WHICH
# packages must report and the compiler pinned WHICH files; these pin that the
# loop above actually walked all of both, so a derivation that collapses between
# the floors and here cannot look clean.
checked=$(( ${#pkgs_graded[@]} + 1 ))
want_checked=$(( expected_count + 1 ))
if (( checked != want_checked )); then
  echo "::error::graded ${checked} package(s) but the manifest and the untagged marker require ${want_checked}."
  status=1
fi
if (( files_graded != gated_count )); then
  echo "::error::graded ${files_graded} file(s) but the compiler reported ${gated_count} in the -tags integration build."
  status=1
fi

if (( status != 0 )); then
  exit 1
fi

echo "Integration suites executed: ${files_graded} file(s) across ${#pkgs_graded[@]} package(s), matching ${manifest}; $(grep -c '^--- PASS' "$log") tests passed in total."
