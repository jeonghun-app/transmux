#!/bin/sh
# Run the unit tests with coverage and fail when the total drops below a floor.
#
# This runs inside the Go build container (`make cover`), so it sticks to POSIX
# sh and the busybox tools in the Alpine image. It writes three files to OUTDIR:
#
#   cover.out   the coverage profile, for `go tool cover -func` or `-html`
#   test.log    the `go test` output the per-package figures are read from
#   summary.md  per-package and total coverage as a Markdown table; CI appends
#               it to the job summary
#
# The summary is written before the floor is checked, so a failing run still
# shows which package lost coverage.
#
# Only the default build is measured. Tests behind the ffmpeg build tag
# (`make test-ffmpeg`) are not included, so the floor is a floor for the unit
# tests that `make test` runs.
#
# Environment: COVER_MIN  minimum total statement coverage, in percent
# Usage:       scripts/coverage.sh OUTDIR

set -eu

outdir=${1:?usage: scripts/coverage.sh OUTDIR}
min=${COVER_MIN:-}
case $min in
  '' | *[!0-9.]* | *.*.* | .*)
    echo "invalid COVER_MIN '$min': must be a percentage such as 69 or 69.5" >&2
    exit 2
    ;;
esac

profile="$outdir/cover.out"
log="$outdir/test.log"
summary="$outdir/summary.md"

# Not piped through tee: POSIX sh has no pipefail, and tee's exit status would
# hide a failing test.
status=0
go test ./... -count=1 -coverprofile="$profile" > "$log" 2>&1 || status=$?
cat "$log"
if [ "$status" -ne 0 ]; then
  exit "$status"
fi

total=$(go tool cover -func="$profile" | awk '$1 == "total:" { sub(/%$/, "", $NF); print $NF }')
if [ -z "$total" ]; then
  echo "no total in $profile" >&2
  exit 1
fi

if awk -v t="$total" -v m="$min" 'BEGIN { exit !(t + 0 >= m + 0) }'; then
  result=pass
else
  result=fail
fi

module=$(go list -m)
{
  echo '### Unit test coverage'
  echo
  echo '| Package | Statements covered |'
  echo '|---|---:|'
  # Packages without test files are listed at 0.0%, which is the point: they
  # count against the total rather than silently dropping out of it.
  awk -v module="$module" '
    /coverage: [0-9.]+% of statements/ {
      pkg = ""
      pct = ""
      for (i = 1; i <= NF; i++) {
        if ($i == module) pkg = "."
        else if (index($i, module "/") == 1) pkg = substr($i, length(module) + 2)
        if ($i == "coverage:") pct = $(i + 1)
      }
      printf "| `%s` | %s |\n", pkg, pct
    }' "$log"
  printf '| **Total** | **%s%%** |\n' "$total"
  echo
  # The backticks are Markdown code spans, not command substitution.
  # shellcheck disable=SC2016
  printf 'Floor (`COVER_MIN`): %s%%. Result: **%s**.\n' "$min" "$result"
} > "$summary"

if [ "$result" = fail ]; then
  echo "total coverage ${total}% is below COVER_MIN ${min}%: add tests for the change rather than lowering the floor" >&2
  exit 1
fi
echo "total coverage ${total}% (COVER_MIN ${min}%)"
