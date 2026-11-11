#!/usr/bin/env bash
# Exercise bd-lint-claim.sh against a stub bd emitting upstream's documented JSON shapes.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d); cp "$HERE/bd-stub" "$TMP/bd"; chmod +x "$TMP/bd"
export PATH="$TMP:$PATH"
L="$HERE/../bd-lint-claim.sh"
rc=0
"$L" bd-good --actor alex --machine debian-box >/dev/null; [ $? -eq 0 ] || { echo "FAIL: bd-good should be workable"; rc=1; }
"$L" bd-bad  --actor alex --machine debian-box >/dev/null; [ $? -eq 1 ] || { echo "FAIL: bd-bad should not be workable"; rc=1; }
n=$("$L" bd-bad --actor alex --json | jq '.fails|length'); [ "$n" = 5 ] || { echo "FAIL: expected 5 fails on bd-bad, got $n"; rc=1; }
# Row 8 flags needs-human and, during the transition, the legacy needs:human.
row8() { "$L" "$1" --actor alex --machine debian-box --json | jq -r '[.fails[] | select(startswith("8 "))] | length'; }
[ "$(row8 bd-good)" = 0 ] || { echo "FAIL: bd-good should pass row 8"; rc=1; }
[ "$(row8 bd-bad)" = 1 ] || { echo "FAIL: row 8 should flag needs-human on bd-bad"; rc=1; }
[ "$(row8 bd-legacy)" = 1 ] || { echo "FAIL: row 8 should flag the legacy needs:human on bd-legacy"; rc=1; }
n=$("$L" bd-legacy --actor alex --machine debian-box --json | jq '.fails|length'); [ "$n" = 1 ] || { echo "FAIL: bd-legacy should fail only row 8, got $n fails"; rc=1; }
[ $rc -eq 0 ] && echo "ok: lint stub tests pass"
rm -rf "$TMP"; exit $rc
