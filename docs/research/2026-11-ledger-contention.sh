#!/usr/bin/env bash
# Measure ledger contention: several bops agents per machine, several
# machines, one remote. Each agent loops claim -> note -> land -> release over
# beads of its own, as /bead-loop agents do, so every write races the others
# on the one refs/dolt/data ref (and, within a machine, on the one embedded
# Dolt store). Results: 2026-11-ledger-contention.md.
#
# usage (measure = bash 2026-11-ledger-contention.sh):
#   measure local  <workdir> [machines=2] [agents=4] [iters=5]
#       Everything on this host: a bare remote in <workdir>, one clone per
#       "machine", then the report. The usual run.
#   measure seed   <workdir> <remote-url> <beads>
#       Seed an EMPTY scratch git repository (never a real project) with a
#       ledger of <beads> beads. Run once, from one machine. Refuses a remote
#       that has any ref, or that it cannot read.
#   measure run    <workdir> <remote-url> <first-machine> <machines> <agents> <iters> [start-epoch]
#       Clone the seeded remote once per local "machine" (numbered from
#       <first-machine>) and run <agents> agents in each. Give every real
#       machine its own <first-machine> and the same <agents>, <iters> and
#       start-epoch (date +%s of a moment a minute ahead) to start together.
#       Refuses a ledger that seed did not make.
#   measure verify <workdir> <remote-url> [other-workdir...]
#       After every run has finished: a fresh clone checks each bead worked in
#       <workdir> and the other workdirs kept its note, landed record and
#       release (lost writes).
#   measure report <workdir>...
#       Latency and failure tables from one or more workdirs' results.
#   measure pulls  <workdir> [case...]
#       Same-row pulls: for each case, on a new local bare remote and two
#       clones, clone A writes a claim display and does not push, clone B
#       edits the same bead and pushes, then A runs bd dolt pull and
#       bd dolt push, and the remote is read again after each side's next
#       write. A case whose pull stops runs again with --strategy ours and
#       theirs; with MEASURE_COMMIT=1 every case runs again with
#       bd dolt commit between A's pull and its push. Needs dolt on PATH.
#       Results: <workdir>/pulls.tsv and <workdir>/cases/<case>/. Cases
#       (default all): control takeover person labels close comment
#       takeover-release takeover-first takeover-multi park delete
#       delete-today.
#
# Env: MEASURE_DELAY_MS (default 0, whole milliseconds) sleeps before every
# git fetch, push and ls-remote, bops's claim-ref traffic and the git calls
# Dolt makes inside bd dolt pull/push alike, to emulate a slower link.
#
# Needs bash, bd, bops, git, jq and perl on PATH. <workdir> must be new, or
# one this script made. Writes inside <workdir> and the remote:
# XDG_STATE_HOME, XDG_CONFIG_HOME and TMPDIR point inside <workdir>, and every
# BEADS_*, BD_* and BOPS_* variable is unset, so this clone's bops state
# (~/.local/state/bops) and any configured bd server are not touched. Your
# git config (credentials, hooks, url rewrites) still applies, and bd and
# Dolt still queue their usage events under ~/.beads and ~/.dolt.
set -u

die() { echo "measure: $*" >&2; exit 1; }
q() { "$@" >>"${W:-/dev/null}/setup.log" 2>&1 || die "setup failed: $* (see ${W:-.}/setup.log)"; }
num() { case $2 in '' | *[!0-9]*) die "$1 must be a whole number, not '$2'" ;; esac; }
now() { perl -MTime::HiRes=time -e 'printf "%.3f", time'; }
for t in bd bops git jq perl; do command -v "$t" >/dev/null || die "$t not on PATH"; done
REAL_BD=$(command -v bd)
REAL_GIT=$(command -v git)
shopt -s nullglob

# common WORKDIR: absolute workdir, shims, isolated identity and state.
common() {
	local f v
	case ${MEASURE_DELAY_MS:-0} in '' | *[!0-9]*) die "MEASURE_DELAY_MS must be whole milliseconds" ;; esac
	mkdir -p "$1" || die "cannot create $1"
	W=$(cd "$1" && pwd)
	"$REAL_GIT" -C "$W" rev-parse --is-inside-work-tree >/dev/null 2>&1 && die "$W is inside a git work tree: use a scratch directory"
	for f in "$W"/* "$W"/.[!.]*; do
		case ${f##*/} in
		bin | config | state | tmp | seed | m[0-9]* | verify | verify.tsv | verify.c | calls.tsv | started | finished | shape | origin.git | setup.log | cases | pulls.tsv) ;;
		*) die "$W holds $f, which this script did not make: use a new directory" ;;
		esac
	done
	mkdir -p "$W/bin" "$W/config" "$W/tmp"
	for v in $(env | awk -F= '/^(BEADS|BD|BOPS)_[A-Za-z0-9_]*=/ {print $1}'); do unset "$v"; done
	export XDG_STATE_HOME="$W/state" XDG_CONFIG_HOME="$W/config" TMPDIR="$W/tmp" \
		GIT_AUTHOR_NAME=measure GIT_AUTHOR_EMAIL=measure@example.invalid \
		GIT_COMMITTER_NAME=measure GIT_COMMITTER_EMAIL=measure@example.invalid \
		MEASURE_CALLS="$W/calls.tsv" MEASURE_DELAY_MS="${MEASURE_DELAY_MS:-0}"
	shims
	export PATH="$W/bin:$PATH"
}

# shims writes bd and git wrappers into $W/bin and appends one row per call
# to $MEASURE_CALLS: tool, op (MEASURE_OP: the bops command that made the
# call), verb, start, secs, exit, lock error, rejected push.
#
# bd runs as a child: the wrapper passes on the SIGTERM bops sends when a
# command's budget runs out, so bd stops as it would unwrapped, and the call
# is still logged. git (network verbs only: fetch, push, ls-remote) logs
# first, sleeps MEASURE_DELAY_MS, then execs git, so a kill reaches git
# itself; its row has no time or exit.
shims() {
	cat >"$W/bin/bd" <<EOF
#!/bin/sh
s=\$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
o=\$(mktemp) e=\$(mktemp)
pid=
trap '[ -n "\$pid" ] && kill -TERM \$pid 2>/dev/null' TERM INT
"$REAL_BD" "\$@" <&0 >"\$o" 2>"\$e" &
pid=\$!
while :; do
	wait \$pid
	rc=\$?
	kill -0 \$pid 2>/dev/null || break
done
cat "\$o"; cat "\$e" >&2
d=\$(perl -MTime::HiRes=time -e 'printf "%.3f", time - \$ARGV[0]' "\$s")
lk=0; nf=0
grep -qiE 'database is locked|locked by another dolt process|lock already held by another process|lock timeout exceeded' "\$o" "\$e" && lk=1
grep -qiE 'non-fast-forward|\\[rejected\\]|stale info|fetch first' "\$o" "\$e" && nf=1
case "\$1" in dolt) v="dolt \$2" ;; *) v="\$1" ;; esac
printf 'bd\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "\${MEASURE_OP:--}" "\$v" "\$s" "\$d" "\$rc" "\$lk" "\$nf" >>"\${MEASURE_CALLS:-/dev/null}"
rm -f "\$o" "\$e"
exit \$rc
EOF
	cat >"$W/bin/git" <<EOF
#!/bin/sh
v=
skip=0
for a in "\$@"; do
	if [ \$skip = 1 ]; then skip=0; continue; fi
	case "\$a" in
	-C | -c | --git-dir | --work-tree | --namespace) skip=1 ;;
	-*) ;;
	*) v=\$a; break ;;
	esac
done
case "\$v" in
push | fetch | ls-remote)
	printf 'git\t%s\t%s\t%s\t-\t-\t0\t0\n' "\${MEASURE_OP:--}" "\$v" "\$(perl -MTime::HiRes=time -e 'printf "%.3f", time')" >>"\${MEASURE_CALLS:-/dev/null}"
	[ "\${MEASURE_DELAY_MS:-0}" -gt 0 ] && sleep "\$(awk "BEGIN{print \${MEASURE_DELAY_MS}/1000}")"
	;;
esac
exec "$REAL_GIT" "\$@"
EOF
	chmod +x "$W/bin/bd" "$W/bin/git"
}

# seed REMOTE BEADS: give an empty remote a main branch and a ledger whose
# every bead is titled "contention NNNN", which run checks for.
seed() {
	local remote=$1 n=$2 i refs
	num beads "$n"
	refs=$("$REAL_GIT" ls-remote "$remote" 2>&1) || die "cannot read $remote: $refs"
	[ -z "$refs" ] || die "$remote is not empty: seed only a new scratch repository"
	rm -rf "$W/seed"
	q git clone -q "$remote" "$W/seed"
	cd "$W/seed" || die "no seed clone"
	q git commit -q --allow-empty -m "contention measurement"
	q git push -q origin HEAD:refs/heads/main
	q bd init --quiet --prefix ct
	i=0
	while [ "$i" -lt "$n" ]; do
		q bd create --title "$(printf 'contention %04d' "$i")" --type task --priority 2 \
			--description "Measurement bead." --acceptance "Claimed, noted, landed and released."
		i=$((i + 1))
	done
	q bd dolt push
	echo "seeded $remote with $n beads"
}

# clone_machine REMOTE G: clone G of the remote, with its own Dolt store and
# bops state, and its bead title -> id map. Refuses a ledger seed did not
# make: one bead not titled "contention NNNN" with a ct- id.
clone_machine() {
	local remote=$1 g=$2 d="$W/m$2"
	rm -rf "$d"
	q git clone -q "$remote" "$d/clone"
	(cd "$d/clone" && q bd bootstrap --yes && chmod 700 .beads) || exit 1
	(cd "$d/clone" && bd list --json --all --limit 0) | jq -r '.[] | [.title, .id] | @tsv' >"$d/beads.tsv" ||
		die "machine $g: cannot list beads"
	[ -s "$d/beads.tsv" ] || die "machine $g: no beads after bootstrap"
	awk -F'\t' '$1 !~ /^contention [0-9][0-9][0-9][0-9]$/ || $2 !~ /^ct-/ {bad = 1} END {exit bad}' "$d/beads.tsv" ||
		die "$remote holds a ledger seed did not make: refusing to write to it"
}

# bops_timed G A I BEAD CMD [ARGS...]: run one bops command for agent A of
# machine G and append its result row. Sets OUTCOME.
bops_timed() {
	local g=$1 a=$2 i=$3 bead=$4 cmd=$5 s e rc out lp rp warn
	shift 5
	out="$W/m$g/runs/a$a-i$i-$cmd"
	s=$(now)
	MEASURE_OP="m$g-a$a-i$i-$cmd" bops "$cmd" "$@" >"$out.json" 2>"$out.err"
	rc=$?
	e=$(now)
	OUTCOME=unparsed
	[ -s "$out.json" ] && OUTCOME=$(jq -rs 'last | .outcome // "-"' "$out.json" 2>/dev/null || echo unparsed)
	lp=$(jq -rs 'last | if .ledger_pending // false then 1 else 0 end' "$out.json" 2>/dev/null || echo -)
	rp=$(jq -rs 'last | if .ref_pending // false then 1 else 0 end' "$out.json" 2>/dev/null || echo -)
	warn=$(jq -rs 'last | (.warnings // []) | join(" | ")' "$out.json" 2>/dev/null | cut -c1-200 | tr '\t\n' '  ')
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$g" "$a" "$i" "$bead" "$cmd" "$s" \
		"$(perl -e 'printf "%.3f", $ARGV[1]-$ARGV[0]' "$s" "$e")" "$rc" "$OUTCOME" "$lp" "$rp" "$warn" >>"$W/m$g/results.tsv"
}

# agent G A AGENTS ITERS: one /bead-loop agent of machine G, working its own
# beads in turn: claim, note, land (no forge, no --verify), release done.
agent() {
	local g=$1 a=$2 agents=$3 iters=$4 i idx bead
	cd "$W/m$g/clone" || exit 1
	export BOPS_RUN="agent$a" BOPS_MACHINE="machine$g" BEADS_ACTOR="dev$g" XDG_STATE_HOME="$W/m$g/state"
	i=0
	while [ "$i" -lt "$iters" ]; do
		idx=$(((g * agents + a) * iters + i))
		bead=$(awk -F'\t' -v t="$(printf 'contention %04d' "$idx")" '$1 == t {print $2}' "$W/m$g/beads.tsv")
		[ -n "$bead" ] || { echo "machine $g agent $a: no bead $idx" >&2; break; }
		bops_timed "$g" "$a" "$i" "$bead" claim "$bead"
		if [ "$OUTCOME" = claimed ]; then
			printf 'Iteration %s of agent %s.\n\nNEXT: land and release.\n' "$i" "$a" | bops_timed "$g" "$a" "$i" "$bead" note "$bead"
			bops_timed "$g" "$a" "$i" "$bead" land "$bead" --forge none \
				--ref "https://example.invalid/mr/$idx" --sha "$(printf '%040x' "$((idx + 1))")"
			if [ "$OUTCOME" = landed ]; then
				bops_timed "$g" "$a" "$i" "$bead" release "$bead" --reason "done"
			else
				bops_timed "$g" "$a" "$i" "$bead" release "$bead" --reason abandoned
			fi
		fi
		i=$((i + 1))
	done
	# As an agent's session ends: sync pushes what its failed pushes left.
	bops_timed "$g" "$a" "$iters" - sync
}

# run_machines REMOTE FIRST MACHINES AGENTS ITERS [START]
run_machines() {
	local remote=$1 first=$2 n=$3 agents=$4 iters=$5 start=${6:-0} g a pids=() need m p
	num first-machine "$first"; num machines "$n"; num agents "$agents"; num iters "$iters"; num start-epoch "$start"
	for m in "$W"/m[0-9]*; do die "$W already holds $m from an earlier run: use a new directory"; done
	need=$(((first + n) * agents * iters))
	g=$first
	while [ "$g" -lt $((first + n)) ]; do
		clone_machine "$remote" "$g"
		[ "$(wc -l <"$W/m$g/beads.tsv")" -ge "$need" ] || die "$remote has fewer than the $need beads this shape needs"
		mkdir -p "$W/m$g/runs"
		printf 'machine\tagent\titer\tbead\tcmd\tstart\tsecs\texit\toutcome\tledger_pending\tref_pending\twarnings\n' >"$W/m$g/results.tsv"
		g=$((g + 1))
	done
	if [ "$start" -gt 0 ]; then
		local wait=$((start - $(date +%s)))
		if [ "$wait" -gt 0 ]; then
			echo "starting in ${wait}s"
			sleep "$wait"
		else
			echo "measure: warning: start time passed ${wait#-}s ago; starting late, out of step with the other machines" >&2
		fi
	fi
	: >"$MEASURE_CALLS"
	# Background agents ignore SIGINT: stop them and the bops command each is
	# running with the script. A bd call already started may still finish.
	trap 'for p in "${pids[@]}"; do pkill -TERM -P "$p" 2>/dev/null; done; kill "${pids[@]}" 2>/dev/null; wait; exit 130' INT TERM
	date +%s >"$W/started"
	g=$first
	while [ "$g" -lt $((first + n)) ]; do
		a=0
		while [ "$a" -lt "$agents" ]; do
			(agent "$g" "$a" "$agents" "$iters") &
			pids+=($!)
			a=$((a + 1))
		done
		g=$((g + 1))
	done
	wait "${pids[@]}"
	trap - INT TERM
	date +%s >"$W/finished"
	printf '%s\t%s\t%s\t%s\t%s\n' "$first" "$n" "$agents" "$iters" "$MEASURE_DELAY_MS" >"$W/shape"
}

# verify REMOTE [WORKDIR...]: a fresh clone reads every bead a result row of
# $W or the other workdirs names. A write is lost when its command reported
# it done (note: noted; land: landed; release: released) and the remote does
# not have it after every agent's sync. A land that failed has nothing to
# keep; its bead was released abandoned. A bead verify cannot read is counted
# as unreadable, not as kept.
verify() {
	local remote=$1 b d results=()
	shift
	for d in "$W" "$@"; do
		d=$(cd "$d" && pwd) || die "no workdir $d"
		results+=("$d"/m*/results.tsv)
	done
	[ "${#results[@]}" -gt 0 ] || die "no results.tsv under $W or $*"
	rm -rf "$W/verify"
	q git clone -q "$remote" "$W/verify"
	cd "$W/verify" || die "no verify clone"
	q bd bootstrap --yes
	chmod 700 .beads
	{
		printf 'bead\tnoted\tlanded\treleased\tassignee\n'
		for b in $(awk -F'\t' 'FNR > 1 && $5 == "claim" && $9 == "claimed" {print $4}' "${results[@]}" | sort -u); do
			# bd show --json carries no comments on bd 1.3.4; bd comments does.
			if bd comments "$b" --json >"$W/verify.c" 2>/dev/null &&
				bd show "$b" --json 2>/dev/null | jq -er --arg b "$b" --slurpfile c "$W/verify.c" '(if type == "array" then .[0] else . end) as $i |
					[$b,
					 ([$c[0][]?.text | select(test("^bops:note"))] | length),
					 (($i.metadata.bops.landed // []) | length),
					 ([$c[0][]?.text | select(test("^bops:released"))] | length),
					 ($i.assignee // "")] | @tsv'; then
				:
			else
				printf '%s\terr\terr\terr\t\n' "$b"
			fi
		done
	} >"$W/verify.tsv"
	awk -F'\t' 'FILENAME != V { if (FNR > 1 && ($5 == "note" || $5 == "land" || $5 == "release")) o[$4, $5] = $9; next }
		FNR > 1 && $2 == "err" { n++; u++; next }
		FNR > 1 {n++; if (o[$1, "note"] == "noted" && $2 < 1) a++; if (o[$1, "land"] == "landed" && $3 < 1) b++
			if (o[$1, "release"] == "released" && $4 < 1) c++; if ($5 != "") d++; if (o[$1, "land"] != "landed") f++}
		END {printf "verify: %d beads (%d lands failed, %d unreadable); lost after sync: note %d, landed record %d, release %d; still assigned %d\n", n, f, u, a, b, c, d}' \
		V="$W/verify.tsv" "${results[@]}" "$W/verify.tsv"
}

# report WORKDIR...: per-command latency and outcomes, the ledger pushes
# behind them, and where the time goes. Ops are keyed by workdir too, so two
# local runs' machine 0 never merge.
report() {
	local d results=() calls=()
	for d in "$@"; do
		[ -d "$d" ] || die "no workdir $d"
		results+=("$d"/m*/results.tsv)
		[ -f "$d/calls.tsv" ] && calls+=("$d/calls.tsv")
		[ -f "$d/shape" ] && printf 'shape %s: first machine, machines, agents, iters, delay_ms = %s\n' "$d" "$(tr '\t' ' ' <"$d/shape")"
		[ -f "$d/started" ] && [ -f "$d/finished" ] && echo "wall: $(($(cat "$d/finished") - $(cat "$d/started"))) s"
	done
	[ "${#results[@]}" -gt 0 ] || die "no results.tsv under $*"
	echo
	echo "command  n   ok  p50    p95    max    outcomes (not ok)  ledger_pending  ref_pending  at-60s-budget"
	awk -F'\t' 'FNR > 1 {
		c = $5; n[c]++; t[c, n[c]] = $7
		ok = ($8 == 0 && ($9 == "claimed" || $9 == "noted" || $9 == "landed" || $9 == "released"))
		if (ok) k[c]++; else bad[c] = bad[c] $9 "/" $8 " "
		if ($10 == 1) p[c]++; if ($11 == 1) r[c]++; if ($7 >= 59.5) b[c]++
	}
	function pct(c, q,   i, j, m, v, a) {
		m = n[c]; for (i = 1; i <= m; i++) a[i] = t[c, i] + 0
		for (i = 2; i <= m; i++) { v = a[i]; for (j = i - 1; j >= 1 && a[j] > v; j--) a[j + 1] = a[j]; a[j + 1] = v }
		i = int(q * m + 0.999999); if (i < 1) i = 1; if (i > m) i = m; return a[i]
	}
	END { split("claim note land release", o, " ")
		for (x = 1; x <= 4; x++) { c = o[x]; if (!(c in n)) continue
			printf "%-8s %3d %4d %6.2f %6.2f %6.2f  %s %d %d %d\n", c, n[c], k[c], pct(c, .5), pct(c, .95), pct(c, 1), (bad[c] == "" ? "-" : bad[c]), p[c], r[c], b[c] }
	}' "${results[@]}"
	[ "${#calls[@]}" -gt 0 ] || return 0
	echo
	echo "ledger pushes per bops command (bd dolt push calls; retries = calls - 1;"
	echo "gave up = the last push failed: five rejections, or the budget stopped it)"
	awk -F'\t' '$2 == "-" {next}
	$1 == "bd" && $3 == "dolt push" {
		split($2, f, "-"); c = f[4]; key = FILENAME ":" $2
		calls[key]++; cmd[key] = c; last[key] = $6; if ($6 != 0) fail[c]++; if ($8 == 1) rej[c]++; tot[c]++
	}
	$1 == "bd" && $7 == 1 { lock[$3]++; lockall++ }
	$1 == "bd" && $6 != 0 && $3 != "dolt push" { nz[$3]++ }
	$1 == "bd" { bdcalls++ }
	$1 == "git" { git[$3]++ }
	END {
		for (k in calls) { c = cmd[k]; ops[c]++; r = calls[k] - 1; if (r > mx[c]) mx[c] = r; if (r > 0) withr[c]++; if (r >= 4) all5[c]++; if (last[k] != 0) gave[c]++ }
		printf "%-8s %5s %7s %9s %9s %8s %8s %8s %8s\n", "command", "ops", "pushes", "w/retry", "max retr", "5 tries", "failed", "rejected", "gave up"
		for (c in ops) printf "%-8s %5d %7d %9d %9d %8d %8d %8d %8d\n", c, ops[c], tot[c], withr[c], mx[c], all5[c], fail[c], rej[c], gave[c]
		printf "\nbd calls %d; database-lock errors %d", bdcalls, lockall
		for (v in lock) printf "; %s %d", v, lock[v]
		printf "\nother bd calls that exited non-zero:"
		for (v in nz) printf " %s %d;", v, nz[v]
		printf "\ngit network calls:"
		for (v in git) printf " %s %d;", v, git[v]
		printf "\n"
	}' "${calls[@]}"
	# Where a command's time goes: mean seconds per command in bd dolt pull,
	# bd dolt push and the other bd calls; the rest of its wall time is bops
	# itself and its claim-ref git calls.
	echo
	echo "mean seconds per command: total, in bd dolt pull, bd dolt push, other bd"
	awk -F'\t' 'FILENAME ~ /results.tsv$/ { if (FNR > 1) { wall[$5] += $7; n[$5]++ }; next }
		$2 == "-" { next }
		$1 == "bd" { split($2, f, "-"); c = f[4]; if ($3 == "dolt pull") pl[c] += $5; else if ($3 == "dolt push") ps[c] += $5; else ot[c] += $5 }
		END { split("claim note land release", o, " ")
			for (x = 1; x <= 4; x++) { c = o[x]; if (!(c in n)) continue
				printf "%-8s %6.2f %6.2f %6.2f %6.2f\n", c, wall[c] / n[c], pl[c] / n[c], ps[c] / n[c], ot[c] / n[c] } }' "${results[@]}" "${calls[@]}"
}

# ------------------------------------------------------------ same-row pulls
#
# What bd dolt pull does when a claim display that was never pushed meets an
# edit of the same bead row from another clone. Clone A plays the claiming
# machine, clone B another machine; each has its own BEADS_ACTOR. No bops
# runs: the writes are the bd calls bops makes (the claim display in the one
# update the design proposes). Each case has its own copy of the seeded
# remote. The dolt CLI only reads copies of A's store, to name the tables a
# pull left uncommitted (bd 1.3.4 has no bd sql in embedded mode, and
# bd vc status does not list them).

PULL_CASES="control takeover person labels close comment takeover-release takeover-first takeover-multi park delete delete-today"

# pc_claim DIR ID RUN ACTOR MACHINE: a claim display in one update and its
# bops:claimed comment.
pc_claim() {
	(cd "$1" && BEADS_ACTOR=$4 q bd update "$2" --assignee "$4" --status in_progress --force --add-label "run:$3" &&
		BEADS_ACTOR=$4 q bd comments add "$2" "bops:claimed run=$3 actor=$4 machine=$5")
}

# pc_claim_multi DIR ID RUN ACTOR MACHINE: today's claim display, one bd
# call per change (bd update --claim, then the run: label and the comment),
# and the heartbeat bops note writes in the unpushed window.
pc_claim_multi() {
	(cd "$1" && BEADS_ACTOR=$4 q bd update "$2" --claim &&
		BEADS_ACTOR=$4 q bd label add "$2" "run:$3" &&
		BEADS_ACTOR=$4 q bd comments add "$2" "bops:claimed run=$3 actor=$4 machine=$5" &&
		BEADS_ACTOR=$4 q bd heartbeat "$2")
}

# pc_release DIR ID RUN ACTOR: release --reason abandoned's display.
pc_release() {
	(cd "$1" && BEADS_ACTOR=$4 q bd update "$2" --assignee "" --status open &&
		BEADS_ACTOR=$4 q bd label remove "$2" "run:$3" &&
		BEADS_ACTOR=$4 q bd label add "$2" bops:hold &&
		BEADS_ACTOR=$4 q bd comments add "$2" "bops:released run=$3 reason=abandoned op=measure")
}

# pc_snap DIR ID: what the bead shows in DIR, '|'-separated: assignee status
# labels title priority notes updated_at closed_at comments newest-bops-comment.
pc_snap() {
	local c i
	c=$(cd "$1" && bd comments "$2" --json 2>/dev/null) && i=$(cd "$1" && bd show "$2" --json 2>/dev/null) &&
		[ -n "$c" ] && [ -n "$i" ] || { echo unreadable; return; }
	printf '%s' "$i" | jq -er --argjson c "$c" '(if type == "array" then .[0] else . end) as $i |
		[($i.assignee // ""), $i.status, (($i.labels // []) | sort | join(",")), $i.title, ($i.priority | tostring),
		 ($i.notes // ""), $i.updated_at, ($i.closed_at // ""), ($c | length | tostring),
		 ([$c[] | select(.text | test("^bops:")) | .text] | last // "")] | join("|")' 2>/dev/null || echo unreadable
}

# pc_dirty DIR: the tables a copy of DIR's Dolt store shows uncommitted, as
# table:status (status modified, conflict, ...), "none", or "err" when the
# Dolt CLI cannot read the store.
pc_dirty() {
	local t="$W/tmp/dirty" out
	rm -rf "$t"
	cp -R "$1/.beads/embeddeddolt/beads" "$t" 2>/dev/null || { echo err; return; }
	if out=$(cd "$t" && dolt sql -r csv -q 'select table_name, status from dolt_status' 2>/dev/null); then
		printf '%s\n' "$out" | awk -F, 'NR > 1 && NF {s = s (s == "" ? "" : ",") $1 ":" $2} END {print (s == "" ? "none" : s)}'
	else
		echo err
	fi
	rm -rf "$t"
}

# pc_row CASEDIR STEP EXIT SIDE OUTFILE: one row of pulls.tsv.
pc_row() {
	local d=$1
	printf '%s\t%s\t%s\t%s\t%s\t%s\n' "${d##*/}" "$2" "$3" "$( [ -n "$4" ] && pc_dirty "$d/$4" || echo)" \
		"$( [ -n "$4" ] && pc_snap "$d/$4" "$(cat "$d/id")" || echo)" \
		"$( [ -n "$5" ] && tr '\n\r\t' '   ' <"$5" | sed 's/  */ /g' | cut -c1-400 || echo)" >>"$W/pulls.tsv"
}

# pc_after CASEDIR [MODE]: A pulls and pushes; B reads the remote; then B
# writes another bead and pushes, A pulls again, writes another bead and
# pushes, and B reads the remote once more. MODE ours or theirs is the first
# pull's --strategy; MODE commit runs bd dolt commit between that pull and
# its push. Returns the first pull's exit.
pc_after() {
	local d=$1 mode=${2:-} st='' rc first other
	case $mode in ours | theirs) st=$mode ;; esac
	other=$(cat "$d/other")
	(cd "$d/A" && BEADS_ACTOR=devA bd dolt pull ${st:+--strategy "$st"}) >"$d/out" 2>&1
	first=$?
	pc_row "$d" "A pull${st:+ --strategy $st}" "$first" A "$d/out"
	if [ "$mode" = commit ]; then
		(cd "$d/A" && BEADS_ACTOR=devA bd dolt commit) >"$d/out" 2>&1
		pc_row "$d" "A bd dolt commit" "$?" A "$d/out"
	fi
	(cd "$d/A" && BEADS_ACTOR=devA bd dolt push) >"$d/out" 2>&1
	pc_row "$d" "A push" "$?" "" "$d/out"
	(cd "$d/B" && BEADS_ACTOR=devB bd dolt pull) >"$d/out" 2>&1
	pc_row "$d" "B pull: remote after A's push" "$?" B "$d/out"
	(cd "$d/B" && BEADS_ACTOR=devB bd comments add "$other" "unrelated write on B" && BEADS_ACTOR=devB bd dolt push) >"$d/out" 2>&1
	rc=$?
	(cd "$d/A" && BEADS_ACTOR=devA bd dolt pull) >>"$d/out" 2>&1
	pc_row "$d" "A pull again (B push $rc)" "$?" A "$d/out"
	(cd "$d/A" && BEADS_ACTOR=devA bd comments add "$other" "unrelated write on A" && BEADS_ACTOR=devA bd dolt push) >"$d/out" 2>&1
	pc_row "$d" "A next write and push" "$?" A "$d/out"
	(cd "$d/B" && BEADS_ACTOR=devB bd dolt pull) >"$d/out" 2>&1
	pc_row "$d" "B pull: remote after A's next push" "$?" B "$d/out"
	return "$first"
}

# pc_case NAME [MODE]: one case on its own copy of the seeded remote, run by
# pc_after in MODE. Returns A's first pull's exit.
pc_case() {
	local name=$1 st=${2:-} d="$W/cases/$1${2:+.$2}" id other side
	rm -rf "$d"
	mkdir -p "$d"
	cp -R "$W/origin.git" "$d/origin.git"
	for side in A B; do
		q git clone -q "$d/origin.git" "$d/$side"
		(cd "$d/$side" && q bd bootstrap --yes && chmod 700 .beads) || exit 1
	done
	id=$(cd "$d/A" && bd list --json --all --limit 0 | jq -r '.[] | select(.title == "contention 0000") | .id')
	other=$(cd "$d/A" && bd list --json --all --limit 0 | jq -r '.[] | select(.title == "contention 0001") | .id')
	[ -n "$id" ] && [ -n "$other" ] || die "$name: no seeded beads"
	echo "$id" >"$d/id"
	echo "$other" >"$d/other"
	# Setup steps exit the script on failure (q's die only ends its subshell),
	# so a write that did not happen is never measured as a clean pull.
	if [ "$name" = delete-today ]; then
		# Today's order: A's claim display is pushed at once and B has it when
		# a person deletes the bead; A then writes the row again (land's
		# landed record updates the same row) and meets the delete at its
		# next pull.
		pc_claim "$d/A" "$id" runA devA machineA || exit 1
		(cd "$d/A" && BEADS_ACTOR=devA q bd dolt push && cd "$d/B" && BEADS_ACTOR=devB q bd dolt pull) || exit 1
		(cd "$d/B" && BEADS_ACTOR=devB q bd delete "$id" --force && BEADS_ACTOR=devB q bd dolt push) || exit 1
		sleep 1.2
		(cd "$d/A" && BEADS_ACTOR=devA q bd update "$id" --notes "landed") || exit 1
	elif [ "$name" = takeover-first ]; then
		# B's takeover reaches the remote before A writes its display (A has
		# not pulled since), so A's write is the later one.
		pc_claim "$d/B" "$id" runB devB machineB || exit 1
		(cd "$d/B" && BEADS_ACTOR=devB q bd dolt push) || exit 1
		sleep 1.2
		pc_claim "$d/A" "$id" runA devA machineA || exit 1
	else
		if [ "$name" = takeover-multi ]; then
			pc_claim_multi "$d/A" "$id" runA devA machineA || exit 1
		else
			pc_claim "$d/A" "$id" runA devA machineA || exit 1
		fi
		sleep 1.2 # B's writes carry a later updated_at, as they would
		case $name in
		control) (cd "$d/B" && BEADS_ACTOR=devB q bd update "$other" --title "contention 0001 renamed") ;;
		takeover) pc_claim "$d/B" "$id" runB devB machineB ;;
		# B sees the bead open and unassigned, so a real claim --force writes
		# today's display there too.
		takeover-multi) pc_claim_multi "$d/B" "$id" runB devB machineB ;;
		park) (cd "$d/B" && BEADS_ACTOR=devB q bd update "$id" --status blocked) ;;
		delete) (cd "$d/B" && BEADS_ACTOR=devB q bd delete "$id" --force) ;;
		person) (cd "$d/B" && BEADS_ACTOR=devB q bd update "$id" --title "renamed by a person" --priority 1 --notes "a person's note") ;;
		labels) (cd "$d/B" && BEADS_ACTOR=devB q bd label add "$id" area:new && BEADS_ACTOR=devB q bd label remove "$id" area:seed) ;;
		close) (cd "$d/B" && BEADS_ACTOR=devB q bd close "$id" --reason "merged via https://example.invalid/mr/1 ($(printf '%040x' 1))") ;;
		comment) (cd "$d/B" && BEADS_ACTOR=devB q bd comments add "$id" "a person's comment") ;;
		takeover-release)
			pc_claim "$d/B" "$id" runB devB machineB &&
				sleep 1.2 &&
				pc_release "$d/B" "$id" runB devB
			;;
		esac || exit 1
		(cd "$d/B" && BEADS_ACTOR=devB q bd dolt push) || exit 1
	fi
	pc_row "$d" "A before pull" "" A ""
	pc_row "$d" "B before pull" "" B ""
	pc_after "$d" "$st"
}

# pulls [CASE...]: seed one remote with two beads, each labelled area:seed,
# then run each case.
pulls() {
	local c
	command -v dolt >/dev/null || die "dolt not on PATH (pulls reads copies of a store with it)"
	for c in "$@"; do
		case " $PULL_CASES " in *" $c "*) ;; *) die "unknown case '$c' (cases: $PULL_CASES)" ;; esac
	done
	[ -e "$W/origin.git" ] && die "$W already holds a run: use a new directory"
	q git init -q --bare "$W/origin.git"
	(seed "$W/origin.git" 2) || exit 1
	(cd "$W/seed" && ids=$(bd list --json --all --limit 0 | jq -er '.[].id') && [ "$(printf '%s\n' "$ids" | wc -l)" -eq 2 ] &&
		for b in $ids; do q bd label add "$b" area:seed; done && q bd dolt push) || die "cannot label the seeded beads"
	printf 'case\tstep\texit\tuncommitted\tassignee|status|labels|title|priority|notes|updated_at|closed_at|comments|newest bops: comment\toutput\n' >"$W/pulls.tsv"
	[ $# -gt 0 ] || set -- $PULL_CASES
	# A pull that stops is run again from the start with each strategy. With
	# MEASURE_COMMIT=1 each case also runs again with bd dolt commit after
	# the pull.
	for c in "$@"; do
		pc_case "$c" || { pc_case "$c" ours; pc_case "$c" theirs; }
		[ "${MEASURE_COMMIT:-0}" = 1 ] && pc_case "$c" commit
	done
	awk -F'\t' '{printf "%-18s %-36s %-4s %-18s %s\n%57s%s\n", $1, $2, $3, $4, $5, "", $6}' "$W/pulls.tsv"
}

cmd=${1:-}
shift || true
case $cmd in
local)
	[ $# -ge 1 ] || die "usage: local <workdir> [machines] [agents] [iters]"
	common "$1"
	M=${2:-2} A=${3:-4} I=${4:-5}
	[ -e "$W/origin.git" ] && die "$W already holds a run: use a new directory"
	q git init -q --bare "$W/origin.git"
	(seed "$W/origin.git" $((M * A * I))) || exit 1
	run_machines "$W/origin.git" 0 "$M" "$A" "$I"
	(verify "$W/origin.git") || exit 1
	report "$W"
	;;
seed)
	[ $# -eq 3 ] || die "usage: seed <workdir> <remote-url> <beads>"
	common "$1"
	seed "$2" "$3"
	;;
run)
	[ $# -ge 6 ] || die "usage: run <workdir> <remote-url> <first-machine> <machines> <agents> <iters> [start-epoch]"
	common "$1"
	run_machines "$2" "$3" "$4" "$5" "$6" "${7:-0}"
	report "$W"
	;;
verify)
	[ $# -ge 2 ] || die "usage: verify <workdir> <remote-url> [other-workdir...]"
	common "$1"
	r=$2
	shift 2
	verify "$r" "$@"
	;;
report)
	[ $# -ge 1 ] || die "usage: report <workdir>..."
	report "$@"
	;;
pulls)
	[ $# -ge 1 ] || die "usage: pulls <workdir> [case...]"
	common "$1"
	shift
	pulls "$@"
	;;
*) awk 'NR > 1 && !/^#/ {exit} NR > 1 {sub(/^# ?/, ""); print}' "$0"; exit 2 ;;
esac
