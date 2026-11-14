#!/usr/bin/env bash
# The local test runner for gstack's pregate (.gstack/pregate.json "runner").
#
# Takes repo-relative bops/**/*_test.go files and runs go test on each file's
# package, from bops/ (the module). Go tests are package-level, so one file
# selects its whole package. scripts/test/run.sh is a guard in the policy:
# pregate does not count a .sh file as a test.
#
# The two-replica and live-ledger integration suite stays in CI: it needs
# BOPS_INTEGRATION=1 and a pinned bd, so it is unset here and those tests skip.
#
# Ends with "# tests N" and "# fail M" so pregate can count what ran.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
unset BOPS_INTEGRATION

pkgs=()
for f in "$@"; do
  case "$f" in
    bops/*_test.go) d=$(dirname "${f#bops/}"); pkgs+=("./$d") ;;
    # bindings/kestrel/beads_test.clj and the like: no runner here or in CI.
    *) echo "pregate-test: skipped $f (no local runner for it)" >&2 ;;
  esac
done

tests=0 fails=0 rc=0
if ((${#pkgs[@]})); then
  log=$(mktemp)
  # -v prints a --- PASS/FAIL line per top-level test, which the count reads.
  (cd bops && go test -count=1 -v $(printf '%s\n' "${pkgs[@]}" | sort -u)) >"$log" 2>&1
  go_rc=$?
  grep -E '^(---|ok|FAIL|panic:)' "$log"
  ((go_rc)) && { tail -40 "$log"; rc=$go_rc; }
  tests=$((tests + $(grep -cE '^--- (PASS|FAIL|SKIP)' "$log")))
  fails=$((fails + $(grep -cE '^--- FAIL' "$log")))
  rm -f "$log"
fi

echo "# tests $tests"
echo "# fail $fails"
exit "$rc"
