#!/usr/bin/env bash
# bd-lint-claim.sh — pre-claim workability lint for a bead (PROCESS.md §3.6/§3.7)
#
# SUPERSEDED by `bops lint <id>`. Kept for repos and bindings that do not use bops yet.
#
# Usage:  bd-lint-claim.sh <bead-id> [--actor NAME] [--machine LABEL] [--no-sync] [--strict] [--json]
# Exit:   0 workable  |  1 fix the bead first  |  2 tool/shape error
#
# Mechanical rows (1–8) are checked by asking bd itself (`bd ready` membership,
# `bd show` fields). Executable rows (9–12,14,15,19,21) are checked heuristically.
# Judgment rows (13,16,17,20) print WARN only. --strict turns WARN into FAIL.
#
# Requires: bd ≥ 0.60, jq. Field names verified against gastownhall/beads
# internal/types/types.go and docs/reference/json-schema.md @ cbfdafc (2026-11-01).

set -u

ID="${1:-}"; shift || true
ACTOR="${BEADS_ACTOR:-$(git config user.name 2>/dev/null || echo "$USER")}"
MACHINE="${BD_MACHINE_LABEL:-$(hostname -s 2>/dev/null)}"
DO_SYNC=1; STRICT=0; JSON=0

while [ $# -gt 0 ]; do
  case "$1" in
    --actor)   ACTOR="$2"; shift 2 ;;
    --machine) MACHINE="$2"; shift 2 ;;
    --no-sync) DO_SYNC=0; shift ;;
    --strict)  STRICT=1; shift ;;
    --json)    JSON=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

[ -z "$ID" ] && { echo "usage: $0 <bead-id> [--actor NAME] [--machine LABEL] [--no-sync] [--strict] [--json]" >&2; exit 2; }
command -v bd >/dev/null || { echo "bd not on PATH" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq not on PATH" >&2; exit 2; }

FAILS=(); WARNS=(); PASSES=()
fail() { FAILS+=("$1"); }
warn() { if [ "$STRICT" = 1 ]; then FAILS+=("$1"); else WARNS+=("$1"); fi; }
pass() { PASSES+=("$1"); }

# ---------------------------------------------------------------- row 21: sync
if [ "$DO_SYNC" = 1 ]; then
  bd sync --json >/dev/null 2>&1; rc=$?
  case $rc in
    0) pass "21 replica synced" ;;
    3) warn "21 sync exit 3 (push race / busy writer) — transient, retried next tick" ;;
    2) fail "21 sync exit 2 — merge conflict halted; resolve before claiming (bd doctor --fix)" ;;
    4) fail "21 sync exit 4 — stuck dirty working set; bd vc status / bd doctor --fix" ;;
    *) warn "21 sync exit $rc — transport/auth error; replica may be stale" ;;
  esac
else
  warn "21 sync skipped (--no-sync); workability judged against local replica"
fi

# ---------------------------------------------------------------- fetch
SHOW="$(bd show "$ID" --json 2>/dev/null)" || { echo "bd show $ID failed (not found?)" >&2; exit 2; }
B="$(printf '%s' "$SHOW" | jq -c 'if type=="array" then .[0] else . end')"
[ -z "$B" ] || [ "$B" = "null" ] && { echo "empty bd show payload for $ID" >&2; exit 2; }

f() { printf '%s' "$B" | jq -r "$1"; }

STATUS=$(f '.status // ""')
TYPE=$(f '.issue_type // ""')
PRIO=$(f '.priority // "null"')
TITLE=$(f '.title // ""')
DESC=$(f '.description // ""')
ACC=$(f '.acceptance_criteria // ""')
DESIGN=$(f '.design // ""')
NOTES=$(f '.notes // ""')
ASSIGNEE=$(f '.assignee // ""')
DEFER=$(f '.defer_until // ""')
LABELS=$(f '(.labels // []) | join(" ")')
PARENT=$(f '.parent // ""')
GATED=$(f '(.gated_by // []) | length')
DEPS="$(printf '%s' "$B" | jq -c '.dependencies // []')"

# ---------------------------------------------------------------- rows 1–7: bd ready membership (authoritative)
READY_IDS="$(bd ready --json 2>/dev/null | jq -r '.[].id' 2>/dev/null)"
if printf '%s\n' "$READY_IDS" | grep -qx "$ID"; then
  pass "1-7 listed by bd ready (open, unblocked, ungated, not deferred, is_blocked current)"
else
  # Explain which mechanical row is the likely cause.
  [ "$STATUS" != "open" ] && fail "1 status is '$STATUS', not open"
  OPEN_BLOCKS=$(printf '%s' "$DEPS" | jq -r '[.[] | select(.type=="blocks" or .type=="conditional-blocks")] | length')
  [ "$OPEN_BLOCKS" != "0" ] && fail "2 has $OPEN_BLOCKS blocks edge(s); run: bd blocked --json | jq '.[]|select(.id==\"$ID\")'"
  EXT=$(printf '%s' "$DEPS" | jq -r '[.[] | select(.depends_on_id|startswith("external:"))] | length')
  [ "$EXT" != "0" ] && fail "3 has $EXT unsatisfied external: dependency(ies)"
  [ "$GATED" != "0" ] && fail "4 gated: $(f '[.gated_by[] | "\(.id)(\(.type)): \(.reason)"] | join("; ")')"
  [ -n "$DEFER" ] && fail "6 deferred until $DEFER"
  [ ${#FAILS[@]} -eq 0 ] && fail "1-7 not in bd ready and no local cause found — blocked parent, stale is_blocked, or unsynced replica (bd sync; bd blocked)"
fi

# ---------------------------------------------------------------- row 5: assignee / claim pool
if [ -n "$ASSIGNEE" ] && [ "$ASSIGNEE" != "$ACTOR" ]; then
  POOLS="$(bd config get claim.pools 2>/dev/null | tr ',' '\n' | sed 's/^ *//;s/ *$//')"
  if printf '%s\n' "$POOLS" | grep -qx "$ASSIGNEE"; then
    pass "5 assigned to claim pool '$ASSIGNEE' — claimable by $ACTOR"
  else
    fail "5 assigned to '$ASSIGNEE' (you are '$ACTOR'); --claim will refuse (anti-steal)"
  fi
else
  pass "5 unassigned or already yours"
fi

# ---------------------------------------------------------------- row 8: needs-human
if printf '%s' "$LABELS" | grep -qw "needs-human"; then
  fail "8 labeled needs-human — not for autonomous pickup"
else
  pass "8 not needs-human"
fi

# ---------------------------------------------------------------- row 9: title
TLEN=${#TITLE}
FIRST=$(printf '%s' "$TITLE" | awk '{print tolower($1)}' | tr -d ':')
if [ "$TLEN" -lt 12 ]; then
  fail "9 title too short ($TLEN chars): '$TITLE'"
elif printf '%s' "$TITLE" | grep -qiE '^(bug|fix|issue|todo|stuff|misc|update|change|wip)$'; then
  fail "9 title is a bare placeholder: '$TITLE'"
elif ! printf '%s' "$FIRST" | grep -qE '^(add|fix|implement|create|remove|refactor|migrate|investigate|spike|document|write|build|configure|enable|disable|replace|extract|split|merge|upgrade|verify|test|design|define|decide|research|audit|measure|port|wire|expose|validate|generate|clean|drop|rename|move|bump|pin|install|set|support|handle|prevent|allow|deny|cache|index|optimize|reduce|convert|parse|render|export|import|sync|close|open|review|plan|draft|ship|deploy|roll|revert)$'; then
  warn "9 title does not start with a verb: '$TITLE'"
else
  pass "9 title specific and verb-first"
fi

# ---------------------------------------------------------------- row 10: description
DLEN=$(printf '%s' "$DESC" | wc -w | tr -d ' ')
if [ "$DLEN" -lt 15 ]; then
  fail "10 description has $DLEN words — needs problem, context, and why"
elif ! printf '%s' "$DESC" | grep -qiE '\b(because|so that|why|causes|needed|needs|impact|affects|currently|without|otherwise|in order to)\b'; then
  warn "10 description has no 'why' language (because/so that/impact/…)"
else
  pass "10 description present with rationale"
fi

# ---------------------------------------------------------------- row 11/12: acceptance
if [ "$TYPE" = "epic" ]; then
  warn "11 epic — acceptance lives on children; agents do not claim epics (PROCESS §1)"
  fail "14 type is epic — claim a child task instead"
else
  ACC_LINES=$(printf '%s' "$ACC" | grep -cE '\S')
  if [ "$ACC_LINES" -eq 0 ]; then
    fail "11 no acceptance criteria — definition of done missing"
  else
    pass "11 $ACC_LINES acceptance line(s) present"
    # row 12: design masquerading as acceptance
    if printf '%s' "$ACC" | grep -qiE '\b(use|using|via|with|call|implement with|approach|library|regex|batchUpdate|jwt|bcrypt|redis|postgres|dynamodb|lambda|step function)\b'; then
      warn "12 acceptance mentions implementation (use/via/library/…) — move HOW to --design"
    fi
    if ! printf '%s' "$ACC" | grep -qiE '\b(returns?|renders?|passes|persists?|shows?|exists?|responds?|rejects?|accepts?|completes?|fails?|succeeds?|is|are|has|have|can|cannot|no longer|within|under|≤|<=|>=)\b'; then
      warn "11 acceptance lines may not be verifiable — each should be a yes/no outcome"
    fi
  fi
fi

# ---------------------------------------------------------------- row 14: priority
case "$PRIO" in
  0|1|2|3|4) pass "14 priority P$PRIO" ;;
  *) fail "14 priority '$PRIO' invalid" ;;
esac
if [ "$PRIO" = "0" ] || [ "$PRIO" = "1" ]; then
  warn "14 P$PRIO — confirm a human set this (agents propose P0/P1 in notes, PROCESS §3.4)"
fi

# ---------------------------------------------------------------- row 15: handoff note when resumed
if [ -n "$NOTES" ]; then
  LAST_BLOCK=$(printf '%s\n' "$NOTES" | awk -v RS='' '{b=$0} END{print b}')
  if printf '%s' "$LAST_BLOCK" | grep -qE '^(COMPLETED|IN PROGRESS|NEXT|BLOCKER|KEY DECISION|REF):'; then
    NEXT=$(printf '%s' "$LAST_BLOCK" | sed -nE 's/^NEXT:[[:space:]]*(.+)$/\1/p' | head -1)
    if [ -z "$NEXT" ]; then
      fail "15 handoff block present but no NEXT line — resume contract broken"
    elif printf '%s' "$NEXT" | grep -qiE '^(continue|keep going|more work|finish|tbd|todo)\b'; then
      fail "15 NEXT is not a concrete step: '$NEXT'"
    else
      pass "15 handoff NEXT: $NEXT"
    fi
    BLK=$(printf '%s' "$LAST_BLOCK" | sed -nE 's/^BLOCKER:[[:space:]]*(.+)$/\1/p' | head -1)
    if [ -n "$BLK" ] && ! printf '%s' "$BLK" | grep -qiE '^(none|n/a|-|no)\b'; then
      fail "15 handoff declares BLOCKER: $BLK"
    fi
  else
    warn "15 notes present but last block is not in handoff format (COMPLETED/IN PROGRESS/NEXT/BLOCKER)"
  fi
else
  pass "15 fresh bead, no handoff required"
fi

# ---------------------------------------------------------------- row 19: machine label
MLABELS=$(printf '%s' "$LABELS" | tr ' ' '\n' | grep -E '^(gpu-|mac-|linux-|win-|host-|debian-|arch-)' || true)
if [ -n "$MLABELS" ]; then
  if printf '%s\n' "$MLABELS" | grep -qx "$MACHINE"; then
    pass "19 machine label matches '$MACHINE'"
  else
    fail "19 machine-bound to [$(printf '%s' "$MLABELS" | tr '\n' ' ')] but this is '$MACHINE'"
  fi
else
  pass "19 no machine constraint"
fi

# ---------------------------------------------------------------- judgment rows → WARN
# 13 scope
if [ "$TYPE" = "feature" ]; then
  CHILD_N=$(bd list --parent "$ID" --flat --json 2>/dev/null | jq 'length' 2>/dev/null || echo 0)
  [ "${CHILD_N:-0}" -gt 0 ] && warn "13 feature with $CHILD_N children — claim a child, not the feature"
fi
if printf '%s' "$ACC" | grep -cE '\S' | awk '{exit !($1>8)}'; then
  warn "13 $(printf '%s' "$ACC" | grep -cE '\S') acceptance lines — likely larger than one session; split?"
fi
# 16 implementation guide
if [ "$TYPE" != "epic" ] && printf '%s%s' "$DESC" "$DESIGN" | grep -qiE '\b(api|endpoint|sdk|format|schema|protocol|integration|oauth|webhook|dolt|bedrock|cdk)\b'; then
  if ! printf '%s' "$NOTES" | grep -qiE 'IMPLEMENTATION GUIDE|WORKING CODE'; then
    warn "16 looks like non-obvious technical work and has no IMPLEMENTATION GUIDE in notes"
  fi
fi
# 17 provenance
PROV=$(printf '%s' "$DEPS" | jq -r '[.[] | select(.type=="discovered-from" or .type=="parent-child")] | length')
if [ "$PROV" = "0" ] && [ -z "$PARENT" ] && [ "$TYPE" != "epic" ]; then
  warn "17 no parent-child or discovered-from link — why does this exist?"
fi
# 18 direction sanity (cheap): this bead should not be blocking its own parent
if [ -n "$PARENT" ]; then
  INV=$(bd show "$PARENT" --json 2>/dev/null | jq -r --arg id "$ID" '(if type=="array" then .[0] else . end) | [.dependencies[]? | select(.type=="blocks" and .depends_on_id==$id)] | length')
  [ "${INV:-0}" != "0" ] && warn "18 parent $PARENT is blocked by this child — check edge direction"
fi
# 20 parent still wanted
if [ -n "$PARENT" ]; then
  PSTAT=$(bd show "$PARENT" --json 2>/dev/null | jq -r '(if type=="array" then .[0] else . end) | .status')
  [ "$PSTAT" = "closed" ] && fail "20 parent $PARENT is closed — child is orphaned busywork"
  [ "$PSTAT" = "deferred" ] && warn "20 parent $PARENT is deferred"
fi

# ---------------------------------------------------------------- report
RC=0; [ ${#FAILS[@]} -gt 0 ] && RC=1
if [ "$JSON" = 1 ]; then
  jq -n --arg id "$ID" --argjson rc "$RC" \
    --argjson fails "$(printf '%s\n' "${FAILS[@]:-}" | jq -R . | jq -s 'map(select(length>0))')" \
    --argjson warns "$(printf '%s\n' "${WARNS[@]:-}" | jq -R . | jq -s 'map(select(length>0))')" \
    --argjson passes "$(printf '%s\n' "${PASSES[@]:-}" | jq -R . | jq -s 'map(select(length>0))')" \
    '{id:$id, workable:($rc==0), fails:$fails, warns:$warns, passes:$passes}'
else
  echo "== $ID  [$TYPE P$PRIO $STATUS]  $TITLE"
  for p in "${PASSES[@]:-}"; do [ -n "$p" ] && echo "  PASS $p"; done
  for w in "${WARNS[@]:-}";  do [ -n "$w" ] && echo "  WARN $w"; done
  for x in "${FAILS[@]:-}";  do [ -n "$x" ] && echo "  FAIL $x"; done
  if [ $RC -eq 0 ]; then echo "-- workable: bd update $ID --claim"; else echo "-- NOT workable (${#FAILS[@]} fail)"; fi
fi
exit $RC
