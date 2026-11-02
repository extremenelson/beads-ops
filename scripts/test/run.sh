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
[ $rc -eq 0 ] && echo "ok: lint stub tests pass"
rm -rf "$TMP"; exit $rc
