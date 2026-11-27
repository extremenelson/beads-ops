package guard

import "testing"

func TestCheckRefusesRawClaims(t *testing.T) {
	for cmd, want := range map[string]string{
		"bd update bops-1 --claim":                      "bd update --claim",
		"bd update --claim bops-1":                      "bd update --claim",
		"bd update bops-1 --claim=true --json":          "bd update --claim",
		"bd --actor me update bops-1 --claim":           "bd update --claim",
		"bd -C /repo --json update bops-1 --claim":      "bd update --claim",
		"/usr/local/bin/bd update x --claim":            "bd update --claim",
		"cd /repo && bd update x --claim":               "bd update --claim",
		"bd show x; bd update x --claim":                "bd update --claim",
		"bd show x | bd update x --claim":               "bd update --claim",
		"(bd update x --claim)":                         "bd update --claim",
		"echo $(bd update x --claim)":                   "bd update --claim",
		"echo `bd reclaim`":                             "bd reclaim",
		"BEADS_ACTOR=me bd update x --claim":            "bd update --claim",
		"env -u FOO BEADS_ACTOR=me bd update x --claim": "bd update --claim",
		"timeout 30 bd update x --claim":                "bd update --claim",
		"command bd reclaim":                            "bd reclaim",
		"bd reclaim":                                    "bd reclaim",
		"bd reclaim --dry-run":                          "bd reclaim",
		"bd ready --claim --json":                       "bd ready --claim",
		"sh -c 'bd update x --claim'":                   "bd update --claim",
		"bash -lc \"bd reclaim\"":                       "bd reclaim",
		"if true; then bd reclaim; fi":                  "bd reclaim",
		"bd update x \\\n  --claim":                     "bd update --claim",
		"cat <<EOF\nbd reclaim\nEOF\nbd reclaim":        "bd reclaim",
		"b'd' update x --claim":                         "bd update --claim",
		"2>/dev/null bd reclaim":                        "bd reclaim",
		"bd update x --claim 2>&1":                      "bd update --claim",
	} {
		h, ok := Check(cmd)
		if !ok || h.Matched != want {
			t.Errorf("Check(%q) = %+v, %v; want %q", cmd, h, ok, want)
		}
		if ok && h.Use == "" {
			t.Errorf("Check(%q): no bops command to use", cmd)
		}
	}
}

// Redirects, eval, arithmetic and command substitution inside double
// quotes (review findings): each row must be refused.
func TestCheckRefusesThroughShellSyntax(t *testing.T) {
	for cmd, want := range map[string]string{
		"2>&1 bd reclaim":                       "bd reclaim",
		"> out bd reclaim":                      "bd reclaim",
		"&>/dev/null bd reclaim":                "bd reclaim",
		">| out bd reclaim":                     "bd reclaim",
		"eval bd reclaim":                       "bd reclaim",
		"eval 'bd update x --claim'":            "bd update --claim",
		"x=$((1<<2))\nbd reclaim":               "bd reclaim",
		"(( y = x << 1 ))\nbd reclaim":          "bd reclaim",
		"echo $((true) ); bd reclaim":           "bd reclaim",
		"echo $((true) | cat); bd reclaim":      "bd reclaim",
		"n=$(( $(bd reclaim | wc -l) + 1 ))":    "bd reclaim",
		"x=\"$(bd update b-1 --claim --json)\"": "bd update --claim",
		"echo \"id: `bd reclaim`\"":             "bd reclaim",
		"bash -o pipefail -c 'bd reclaim'":      "bd reclaim",
		"coproc bd reclaim":                     "bd reclaim",
		"cat <<A\n$((1<<2))\nA\nbd reclaim":     "bd reclaim",
		"git commit -m \"$(cat <<'EOF'\ndon't do this\nEOF\n)\" && bd reclaim": "bd reclaim",
		"bd list <<< \"x\"\nbd reclaim":                                        "bd reclaim",
		"echo \"<b>\"& bd reclaim":                                             "bd reclaim",
		"x=\"$(echo \"$(bd reclaim)\")\"":                                      "bd reclaim",
	} {
		h, ok := Check(cmd)
		if !ok || h.Matched != want {
			t.Errorf("Check(%q) = %+v, %v; want %q", cmd, h, ok, want)
		}
	}
	for _, cmd := range []string{
		"echo $((1<<2))",
		"git commit -m \"$(cat <<'EOF'\nsubject (with a paren)\n\nnever bd update x --claim\nbd reclaim\nEOF\n)\"",
		"echo \"$(echo bd reclaim)\"",
		"gh pr create --body \"$(cat <<'EOF'\n## Summary\n1) adds the guard\n2) refuses `bd reclaim`\nEOF\n)\"",
		"git commit -m \"$(cat <<'EOF'\nguard: fix :)\nNow `bd reclaim` is refused.\nEOF\n)\"",
		"git commit -m \"$(cat <<'EOF'\nIt's fixed\nEOF\n)\" && echo \"done; bd reclaim is refused\"",
		"echo \"$(echo \")\") && bd reclaim\"",
		"echo \"<b>\"& echo hi",
	} {
		if h, ok := Check(cmd); ok {
			t.Errorf("Check(%q) refused (%+v), want allowed", cmd, h)
		}
	}
}

func TestCheckAllowsEverythingElse(t *testing.T) {
	for _, cmd := range []string{
		"",
		"bops claim x",
		"bops claim --force x",
		"bd update x --status open",
		"bd update x --claim=false",
		"bd ready --json",
		"bd show x --json",
		"bd comments add x 'never bd update --claim'",
		"echo bd update x --claim",
		"echo 'bd reclaim'",
		"echo \"bd update x --claim; bd reclaim\"",
		"git commit -m 'refuse bd reclaim'",
		"bops note x <<'EOF'\nbd update x --claim\nbd reclaim\nEOF",
		"cat <<-END\n\tbd reclaim\n\tEND",
		"command -v bd",
		"# bd reclaim",
		"bd list # bd reclaim",
		"bd update x -- --claim",
		"grep -rn 'bd reclaim' docs",
		"rg reclaim",
		"bd --db reclaim show x",
		"bd reclaim --help",
		"bd update --claim --help",
		"bd ready -h --claim",
		"eval echo bd reclaim",
	} {
		if h, ok := Check(cmd); ok {
			t.Errorf("Check(%q) refused (%+v), want allowed", cmd, h)
		}
	}
}

// Two heredocs on one line: both bodies are skipped, and a claim after them
// is still seen.
func TestCheckTwoHeredocs(t *testing.T) {
	cmd := "cat <<A <<B\nbd reclaim\nA\nbd reclaim\nB\nbd update x --claim"
	h, ok := Check(cmd)
	if !ok || h.Matched != "bd update --claim" {
		t.Fatalf("Check = %+v, %v", h, ok)
	}
	if _, ok := Check("cat <<A <<B\nbd reclaim\nA\nbd reclaim\nB\necho done"); ok {
		t.Fatal("a claim inside a heredoc body was refused")
	}
}
