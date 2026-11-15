#!/bin/sh
# The Claude Code binding is generated: bindings/claude-code and this repo's
# own .claude/skills/bops/SKILL.md must match `bops gen cc` byte for byte.
# Fix: cd bops && go run ./cmd/bops gen cc --out ../bindings/claude-code &&
#      cp ../bindings/claude-code/skills/bops/SKILL.md ../.claude/skills/bops/SKILL.md
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
(cd "$root/bops" && go run ./cmd/bops gen cc --out "$d" >/dev/null)
rc=0
for f in $(cd "$d" && find . -type f | sort); do
	diff -u "$d/$f" "$root/bindings/claude-code/$f" || rc=1
done
diff -u "$d/skills/bops/SKILL.md" "$root/.claude/skills/bops/SKILL.md" || rc=1
exit $rc
