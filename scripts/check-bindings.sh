#!/bin/sh
# The bindings are generated: bindings/claude-code, bindings/codex,
# bindings/clojure and bindings/mcp, and this repo's own installed skills
# (.claude/skills/bops and .agents/skills/bops), must match `bops gen cc`,
# `bops gen codex`, `bops gen clj` and `bops gen mcp` byte for byte.
# Fix, from bops/:
#   go run ./cmd/bops gen cc --out ../bindings/claude-code &&
#   go run ./cmd/bops gen codex --out ../bindings/codex &&
#   go run ./cmd/bops gen clj --out ../bindings/clojure &&
#   go run ./cmd/bops gen mcp --out ../bindings/mcp &&
#   cp ../bindings/claude-code/skills/bops/SKILL.md ../.claude/skills/bops/SKILL.md &&
#   cp ../bindings/codex/skills/bops/SKILL.md ../.agents/skills/bops/SKILL.md
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
rc=0
check() { # target, binding dir, installed skill dir (none for clj and mcp)
	(cd "$root/bops" && go run ./cmd/bops gen "$1" --out "$d/$1" >/dev/null)
	for f in $(cd "$d/$1" && find . -type f | sort); do
		diff -u "$d/$1/$f" "$root/$2/$f" || rc=1
	done
	if [ -n "${3:-}" ]; then
		diff -u "$d/$1/skills/bops/SKILL.md" "$root/$3/SKILL.md" || rc=1
	fi
}
check cc bindings/claude-code .claude/skills/bops
check codex bindings/codex .agents/skills/bops
check clj bindings/clojure
check mcp bindings/mcp
exit $rc
