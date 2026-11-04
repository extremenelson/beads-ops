#!/usr/bin/env bash
# Creates the bops v0.1.0 epic and its 14 tasks in the current repo's beads
# ledger, with blocks deps, acceptance criteria, and bodies via --body-file
# (never text on the command line). Run once from the beads-ops checkout root:
#   BOPS_PLAN_DRY_RUN=1 scripts/plan/v0.1.0-beads.sh   # then again without the env var
# Human-readable mirror: docs/plan/v0.1.0.md
#
# Verified against: nothing yet. Flags assumed from bd 1.3.x help:
#   bd create "<title>" --type task|epic --priority N --body-file F --acceptance "<ac>" --parent <id> --json
#   bd dep add <issue> <depends-on>           # <issue> is blocked by <depends-on>
# If either differs, fix the two helper functions below and re-run with
# BOPS_PLAN_DRY_RUN=1 first.
set -euo pipefail
command -v bd >/dev/null || { echo "bd not on PATH" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq not on PATH" >&2; exit 1; }
DRY="${BOPS_PLAN_DRY_RUN:-0}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

declare -A ID   # key -> bead id

create() { # key type priority title acceptance [parent-key]
  local key="$1" type="$2" prio="$3" title="$4" ac="$5" parent="${6:-}"
  local body="$TMP/$key.md"; cat > "$body"
  local args=(create "$title" --type "$type" --priority "$prio" --body-file "$body" --acceptance "$ac" --json)
  [[ -n "$parent" ]] && args+=(--parent "${ID[$parent]}")
  if [[ "$DRY" == 1 ]]; then echo "bd ${args[*]}"; ID[$key]="dry-$key"; return; fi
  ID[$key]="$(bd "${args[@]}" | jq -r 'if type=="array" then .[0].id else .id end')"
  echo "created $key -> ${ID[$key]}"
}

dep() { # child-key blocker-key  (child is blocked by blocker)
  if [[ "$DRY" == 1 ]]; then echo "bd dep add ${ID[$1]} ${ID[$2]}"; return; fi
  bd dep add "${ID[$1]}" "${ID[$2]}" >/dev/null && echo "dep ${ID[$1]} blocked-by ${ID[$2]}"
}

# ------------------------------------------------------------------ epic
create epic epic 1 "bops v0.1.0 — intent-level command layer over beads" \
"A release binary provides next claim note release land sweep stale lint sync session-start gates deps version per SPEC.md; the Claude Code binding is generated from the manifest; claim/release verified against a live bd 1.3.x ledger; README no longer calls bd pre-1.0." <<'EOF'
Tracks delivery of the Go reference implementation in bops/ to contract 1 as
written in SPEC.md. Children are sized for one Claude Code session each.

Session protocol for every child: bops session-start → bops next → bops claim <id>
→ work in a worktree → tests green → commit with the bead id in the subject →
bops note (ending with a NEXT: paragraph) → bops release --reason done → PR.

KEY DECISION: writes only via bd; no raw SQL; stdlib only unless justified in CONTRIBUTING.md.
REF: SPEC.md, bops/commands.json, bops/testdata/README.md
EOF

# ------------------------------------------------------------- wave 0
create verify task 0 "Verify claim/release bd invocations against a live ledger; add BOPS_INTEGRATION suite" \
"internal/intent has a BOPS_INTEGRATION=1 suite that runs claim and release against a throwaway bd init ledger and passes; testdata/README.md lists each verified bd syntax with the bd version; any mismatch is fixed in internal/intent and the Fake test keys." epic <<'EOF'
Three bd invocations in internal/intent were written from docs, not a live run:

  bd update <id> --claim --if-assignee ''     (empty-string CAS)
  bd update <id> --assignee ''                (clear assignee on release)
  bd label add|remove <id> <label>

Also capture real shapes for `bd show <id> --json` (one-element array?) and
`bd comments <id> --json` (needed by note/stale) into testdata/README.md as
field lists — never commit a real ledger export.

Approach: `bd init` in a temp dir, create two beads, run bops claim/release
through bdx.Exec, assert readback. Gate the suite on BOPS_INTEGRATION=1 so
`go test ./...` stays Dolt-free.

NEXT: run the suite locally with bd 1.3.2, then on a second machine after bd dolt pull.
EOF

create hygiene task 1 "Repo hygiene: README bd >= 1.3, CLAUDE.md session protocol, hooks point at bops" \
"rg 'pre-1.0|0.60' README.md returns nothing; bindings/claude-code/CLAUDE.md.snippet contains the bops session protocol; bindings/claude-code/settings.json SessionStart and Stop hooks call bops session-start and bops sync; docs/PROCESS.md section 9.1 no longer embeds stale copies of those files." epic <<'EOF'
README.md still says bd is pre-1.0 and requires >= 0.60; SPEC requires >= 1.3.
PROCESS.md §9.1 embeds older copies of settings.json and the CLAUDE.md block
than the canonical bindings/claude-code/ files — replace with links.

Add to CLAUDE.md.snippet:
  bops session-start → bops next → bops claim <id> → work → bops note → bops release --reason done

Keep bd-lint-claim.sh for now; mark it superseded by `bops lint` in its header.

NEXT: open PR; no code changes outside docs and bindings/claude-code.
EOF

# ------------------------------------------------------------- wave 1
create note task 1 "bops note: milestone comment via stdin/--file, NEXT: lint, heartbeat" \
"Tests cover text from stdin and from --file; refusal (exit 1, no write) when the bead's run label is not this run; a warning when the last paragraph does not start with NEXT:|BLOCKER:|KEY DECISION:|REF:; bd heartbeat is called after the comment; no bead text ever appears in a bd argument except the fixed event line." epic <<'EOF'
Implements commands.json `note` (SPEC §4, §3.2). Comment body is:

  bops:note run=<ulid>
  <text from stdin or --file, verbatim>

Open question: does `bd comments add` accept --file or stdin? If not, write
the text to a temp file and pass it however bd allows; the principle is that
user text never goes through shell argv from the agent side (bops owns it).
Record the verified syntax in testdata/README.md.

NEXT: implement after the verify bead lands so comment syntax is known.
EOF
dep note verify

create stale task 1 "bops stale: reconstruct last activity from comments + heartbeat; same_machine from run state" \
"Given a fixture with three in_progress beads (live, stale on this machine, stale on another machine) and --older-than 2h, output lists exactly the two stale ones with correct age_minutes and same_machine; --all-machines=false hides the other machine's." epic <<'EOF'
Implements commands.json `stale` (SPEC §4). Last activity = max(updated_at,
heartbeat_at, newest bops:* comment). same_machine compares the run label's
ulid with $XDG_STATE_HOME/bops/<repo>/run.json (current) and a small
history file of past run ids on this machine (append on each new run).

This is the highest-leverage gap in upstream beads: leases are node-local and
never reach Dolt, so cross-machine stale detection only exists here.

NEXT: wire into session-start (count) and next (W06s uses the same clock).
EOF
dep stale verify

create sessionstart task 1 "bops session-start: one compact orientation document for the SessionStart hook" \
"Output for a 100-bead fixture is under 1 kB in JSON; includes mine[], stale count, ready_count, and bd prime text; bindings/claude-code/settings.json SessionStart hook runs it; a failed sync still produces output with ok=false rather than nothing." epic <<'EOF'
Composes sync (no-push), bd prime, stale (count only), and my in_progress list.
This replaces the four separate commands the current hooks fragment runs.
Keep it small: the hook output lands in every session's context.

NEXT: measure token size of the prime text and truncate with a marker if > 600 tokens.
EOF
dep sessionstart note
dep sessionstart stale

create gates task 2 "bops gates + W05: open gates exclude a bead from next" \
"bops gates <id> lists gates with satisfied flags and blocking deps; Next populates LintEnv.OpenGates from bd gate list --json so a bead behind a human gate is excluded with W05 and its id appears in blocked_by_gates." epic <<'EOF'
Implements commands.json `gates` and closes the W05 TODO in internal/intent.
Fixture needed: `bd gate list --json` shape (capture during the verify bead).

NEXT: if bd ready already excludes gated beads, W05 still matters for --include-unworkable explanations.
EOF
dep gates verify

# ------------------------------------------------------------- wave 2
create forge task 1 "internal/forge adapter + bops land" \
"internal/forge exposes is_merged(ref) and ref_sha(ref) with github (gh api), gitlab (glab api) and none implementations selected from git remote or --forge; tests stub gh/glab; bops land writes the bops:landed comment and merges metadata.bops.landed without touching other metadata keys; land never calls the forge unless --verify." epic <<'EOF'
SPEC §7. Nothing outside internal/forge may know what a PR is. `land` records
evidence only; closing is sweep's job.

Metadata merge: read current metadata JSON via bd show, deep-merge only the
`bops` key, write back with `bd update <id> --metadata` (verify the flag
name in the verify bead or here).

NEXT: sweep builds on this.
EOF
dep forge verify

create sweep task 2 "bops sweep: close landed beads whose refs are merged (dry-run default)" \
"Dry-run makes no bd write calls (asserted via the Fake call log); a real run closes beads whose landed ref is_merged, lists pending and unknown separately, and uses the close reason format 'merged via <ref> (<sha>)'." epic <<'EOF'
Only sweep (or a human) closes. Default dry-run so a hook can never close by
accident; `--dry-run=false` is explicit.

NEXT: consider a `--max N` cap for the first real run on a large ledger.
EOF
dep sweep forge

create deps task 2 "bops deps: transitive blockers/dependents and critical path (engine=bd, cte)" \
"On a synthetic 1k-node DAG fixture, --transitive returns the correct closure and --path returns the longest path matching a brute-force oracle test; a cyclic fixture exits 1 naming the cycle; engine=cte uses bd sql with a recursive CTE and produces the same results as engine=bd." epic <<'EOF'
Critical path is computed in Go (Kahn's algorithm over the fetched subgraph),
not by pattern matching — the earlier research showed path enumeration in
query languages fails to finish at scale while a topological pass is ms.

engine=graph is reserved for the later Datalevin/SQLite read model; do not
implement here, but keep the enum so the flag is stable.

NEXT: the 1k-node generator can live in testdata as code, not as a file.
EOF
dep deps verify

# ------------------------------------------------------------- wave 3
create gencc task 1 "bops gen cc: Claude Code skill + hooks fragment from the manifest (golden-tested)" \
"bops gen cc emits skills/bops/SKILL.md (hand-written preamble file + generated command reference), a hooks.json fragment for SessionStart/PreCompact/Stop, and an optional /bops-next command; output is golden-tested; installing it into a scratch repo makes Claude Code list the skill; SKILL.md stays under 2k tokens." epic <<'EOF'
SPEC §8. The preamble lives in bops/gen/templates/cc-preamble.md and is the
only hand-written prose; everything else comes from commands.json so a
contract bump regenerates the reference. Surfaces come from each command's
`surfaces.cc` entry.

NEXT: codex target is the same generator with a different output path; add it in the same PR if trivial.
EOF

create genclj task 2 "bops gen clj: Clojure binding with Malli schemas; retire bindings/kestrel/beads.clj" \
"bops gen clj emits bops.clj with one fn per command shelling out to bops --json and a Malli schema per output derived from commands.json; bb -e \"(require 'bops)\" loads it; bindings/kestrel/beads.clj is deleted and the Kestrel README points at the generated file." epic <<'EOF'
The never-run beads.clj draft is replaced by generated code. Malli schemas are
derived mechanically from the `output` objects in the manifest; `string?`
→ :maybe, arrays → :vector.

NEXT: in-process Go-library access is out of scope; the Clojure side always calls the binary.
EOF
dep genclj gencc

create genmcp task 3 "bops gen mcp + bops serve-mcp: read-only tools by default, --enable-writes, pagination" \
"bops gen mcp emits a tools list equal to the manifest minus kind=write unless --enable-writes; bops serve-mcp speaks stdio MCP and Claude Code connects and lists the tools; results over 20 kB are paginated with a cursor." epic <<'EOF'
SPEC §8 MCP row. Lowest priority: beads' own guidance is CLI+hooks first for
token cost. Keep the server a thin wrapper over the same intent functions.

NEXT: decide whether to vendor a minimal MCP stdio implementation or justify a dependency in CONTRIBUTING.md.
EOF
dep genmcp gencc

# ------------------------------------------------------------- wave 4
create envelope task 2 "bd 2.0 JSON envelope support in internal/ledger" \
"A fixture captured from BD_JSON_ENVELOPE=1 bd ready --json is pinned in testdata; DecodeIssues and DecodeIssue accept both raw and envelope forms and produce identical []Issue for equivalent fixtures; testdata/README.md documents the envelope shape." epic <<'EOF'
bd prints a deprecation notice that --json output changes in 2.0. The
envelope's shape is not guessed; capture it first (sanitize titles) and then
teach ledger.Decode* to unwrap. Until this lands, bops runs with the envelope off.

NEXT: once pinned, consider setting BD_JSON_ENVELOPE=1 in bdx.Exec unconditionally.
EOF
dep envelope verify

create release task 2 "Release pipeline: goreleaser, version ldflags, go install docs, tag v0.1.0" \
"A tagged build produces linux/darwin/windows amd64+arm64 and android/arm64 binaries; bops version prints the tag; README documents go install and the release download; CHANGELOG has a v0.1.0 entry; the binary runs on Linux x86_64 and on Termux arm64." epic <<'EOF'
Ship once the agent loop is complete (note, stale, session-start), landing
works (forge/land), and the Claude Code binding generates. Termux arm64 is a
first-class target for the maintainer's travel setup.

NEXT: after tagging, dogfood on the claude-skills ledger for a week before announcing.
EOF
dep release note
dep release stale
dep release sessionstart
dep release forge
dep release gencc

echo; echo "epic: ${ID[epic]}"; echo "run: bops next   (or: bd ready)"
