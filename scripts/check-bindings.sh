#!/bin/sh
# The agent bindings are generated: bindings/claude-code and bindings/codex,
# and this repo's own installed skills (.claude/skills/bops and
# .agents/skills/bops), must match `bops gen cc` and `bops gen codex` byte
# for byte.
# Fix, from bops/:
#   go run ./cmd/bops gen cc --out ../bindings/claude-code &&
#   go run ./cmd/bops gen codex --out ../bindings/codex &&
#   cp ../bindings/claude-code/skills/bops/SKILL.md ../.claude/skills/bops/SKILL.md &&
#   cp ../bindings/codex/skills/bops/SKILL.md ../.agents/skills/bops/SKILL.md
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
rc=0
check() { # target, binding dir, installed skill dir
	(cd "$root/bops" && go run ./cmd/bops gen "$1" --out "$d/$1" >/dev/null)
	for f in $(cd "$d/$1" && find . -type f | sort); do
		diff -u "$d/$1/$f" "$root/$2/$f" || rc=1
	done
	diff -u "$d/$1/skills/bops/SKILL.md" "$root/$3/SKILL.md" || rc=1
}
check cc bindings/claude-code .claude/skills/bops
check codex bindings/codex .agents/skills/bops
exit $rc
