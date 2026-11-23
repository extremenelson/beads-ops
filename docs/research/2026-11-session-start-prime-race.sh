#!/bin/sh
# Measure bops session-start --hook-json and bd prime --hook-json started
# together on one embedded Dolt store, as Claude Code starts matching
# SessionStart hooks (bops-5qn). Results: 2026-11-session-start-prime-race.md.
#
# usage: sh 2026-11-session-start-prime-race.sh [trials] [workdir] [cases]
# cases defaults to "idle pull writes"; "control" runs the loop writes alone,
# with no hook, to show what the remote change alone does to their push.
# Needs bd, bops and git on PATH. Touches nothing outside workdir: the
# remote is a local bare repository and XDG_STATE_HOME points inside it.
set -u
N=${1:-20}
W=${2:-$(mktemp -d)}
CASES=${3:-idle pull writes}
mkdir -p "$W"
W=$(cd "$W" && pwd)
export XDG_STATE_HOME="$W/state" BEADS_ACTOR=tester GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid \
	GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
unset BOPS_RUN BEADS_DIR
q() { "$@" >/dev/null 2>&1 || { echo "setup failed: $*" >&2; exit 1; }; }

# a is the session's clone; b pushes a change before every trial, so each
# trial's pull has something to merge, as on a ledger other machines write.
q git init -q --bare "$W/origin.git"
q git clone -q "$W/origin.git" "$W/a"
cd "$W/a"
q git commit -q --allow-empty -m init
q git push -q origin HEAD:refs/heads/main
q bd init --quiet --prefix rc
for i in 1 2 3 4 5 6 7 8 9 10; do q bd create --title "bead $i" --type task --priority 2 --description "d" --acceptance "a"; done
q bd dolt push
q git clone -q "$W/origin.git" "$W/b"
(cd "$W/b" && q bd bootstrap --yes)
HELD=$(bd list --json | sed -n 's/.*"id": *"\(rc-[a-z0-9]*\)".*/\1/p' | head -1)
q bops claim "$HELD" # a hold of a's run, so session-start writes too
WRITE=$(bd list --json | sed -n 's/.*"id": *"\(rc-[a-z0-9]*\)".*/\1/p' | sed -n 2p)

OUT="$W/results.tsv"
printf 'case\ttrial\tcommand\texit\tsecs\tok\terror\n' >"$OUT"

# record CASE TRIAL NAME EXIT SECS FILE: ok means a hook document came back.
record() {
	ok=no
	case $3 in
	prime) grep -q '"additionalContext"' "$6" && ok=yes ;;
	# its own document must say it synced, with no warning (a failed pull
	# or bd call inside it is a warning, not an exit code).
	session-start) grep -q '\\"synced\\": true' "$6" && grep -q '\\"warnings\\": \[\]' "$6" && ok=yes ;;
	*) [ "$4" = 0 ] && ok=yes ;;
	esac
	err=$(grep -i -m1 -E 'error|lock|fail|warning|rejected' "$6.err" 2>/dev/null | cut -c1-200 | tr '\t\n' '  ')
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" "$5" "$ok" "$err" >>"$OUT"
}

# timed NAME FILE CMD...: run CMD, write its exit and seconds to FILE.meta.
timed() {
	name=$1 f=$2
	shift 2
	s=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
	"$@" >"$f" 2>"$f.err"
	rc=$?
	e=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
	echo "$rc $(echo "$e - $s" | bc)" >"$f.meta"
}

# A change from another machine, so a's next pull merges.
remote_change() {
	(cd "$W/b" && bd dolt pull >/dev/null 2>&1; bd create --title "from b $1-$2" --type task --priority 3 --description d --acceptance a >/dev/null 2>&1 && bd dolt push >/dev/null 2>&1)
}

for c in $CASES; do
	t=1
	while [ "$t" -le "$N" ]; do
		remote_change "$c" "$t"
		d="$W/runs/$c-$t"
		mkdir -p "$d"
		if [ "$c" != control ]; then
			timed session-start "$d/ss" bops session-start --hook-json &
			timed prime "$d/prime" bd prime --hook-json &
		fi
		case $c in
		pull) timed pull "$d/third" bd dolt pull & ;;
		# what a running /bead-loop does between claims
		writes | control) timed writes "$d/third" sh -c "$(command -v bd) label add $WRITE loop-$t && $(command -v bd) comments add $WRITE 'loop note $t' && $(command -v bd) dolt push" & ;;
		esac
		wait
		if [ -f "$d/ss.meta" ]; then
			set -- $(cat "$d/ss.meta")
			record "$c" "$t" session-start "$1" "$2" "$d/ss"
			set -- $(cat "$d/prime.meta")
			record "$c" "$t" prime "$1" "$2" "$d/prime"
		fi
		if [ -f "$d/third.meta" ]; then
			set -- $(cat "$d/third.meta")
			record "$c" "$t" "$([ "$c" = control ] && echo writes || echo "$c")" "$1" "$2" "$d/third"
		fi
		t=$((t + 1))
	done
done

echo "results: $OUT"
awk -F'\t' 'NR>1 {k=$1" "$3; n[k]++; if ($6!="yes") f[k]++} END {for (k in n) printf "%-24s %2d/%d failed\n", k, f[k]+0, n[k]}' "$OUT" | sort
