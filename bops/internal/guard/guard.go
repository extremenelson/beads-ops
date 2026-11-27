// Package guard finds raw bd claims in a shell command: `bd update --claim`,
// `bd ready --claim` and `bd reclaim`. Each ignores the bops claim lock, so on
// a ledger whose lock is on, `bops guard` refuses them before the harness runs
// the command (SPEC.md §8).
//
// It reads the command the way a shell splits it into simple commands, well
// enough for what an agent types: quotes, escapes, `;`, `&&`, `||`, `|`,
// subshells, command substitution, heredoc bodies (skipped), arithmetic
// expansion, leading assignments and wrappers such as env, command and
// timeout, `sh -c` and eval. It does not expand variables or aliases, read
// sourced scripts, or read a script fed to a shell on stdin (`bash <<EOF`),
// so a claim spelled through one is not seen; the guard is a reminder for an
// agent that forgot, not a sandbox.
package guard

import (
	"path"
	"strings"
)

// Hit is a raw claim the command would run.
type Hit struct {
	Matched string // bd update --claim | bd ready --claim | bd reclaim
	Use     string // what to run instead
}

var (
	updateClaim = Hit{"bd update --claim", "bops claim <id>, or bops claim --force <id> to take over a stale claim"}
	readyClaim  = Hit{"bd ready --claim", "bops next, then bops claim <id>"}
	reclaim     = Hit{"bd reclaim", "bops stale to list idle claims, then bops claim --force <id> (or bops reap)"}
)

// Check returns the first raw claim in cmd, or false when it has none.
func Check(cmd string) (Hit, bool) {
	return check(cmd, 0)
}

// maxDepth bounds `sh -c` nesting.
const maxDepth = 4

func check(cmd string, depth int) (Hit, bool) {
	for _, words := range simpleCommands(cmd) {
		if h, ok := checkWords(words, depth); ok {
			return h, true
		}
	}
	return Hit{}, false
}

// keywords may start a simple command before the program word.
var keywords = map[string]bool{"coproc": true, "!": true, "{": true, "}": true, "if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true}

// wrappers run the rest of their words as a command. Their own options are
// skipped; valued says which take a value as a separate word.
var wrappers = map[string]map[string]bool{
	"env":     {"-u": true, "--unset": true, "-C": true, "--chdir": true, "-S": true},
	"command": {},
	"exec":    {"-a": true},
	"nohup":   {},
	"time":    {},
	"sudo":    {"-u": true, "-g": true, "-C": true, "-D": true, "-h": true, "-p": true, "-U": true},
	"nice":    {"-n": true},
	"timeout": {"-s": true, "--signal": true, "-k": true, "--kill-after": true},
	"xargs":   {"-I": true, "-n": true, "-P": true, "-L": true, "-d": true, "-E": true, "-s": true},
}

// shells take a command string after -c.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// bdValued are bd's global flags that take a separate value word.
var bdValued = map[string]bool{"--actor": true, "--database": true, "--db": true, "-C": true, "--directory": true, "--dolt-auto-commit": true, "--mem-profile": true}

func checkWords(w []string, depth int) (Hit, bool) {
	i := 0
	for i < len(w) {
		word := w[i]
		switch {
		case keywords[word], isAssignment(word):
			i++
			continue
		case isRedirect(word):
			i++
			if redirectNeedsTarget(word) { // the target is the next word
				i++
			}
			continue
		}
		prog := path.Base(word)
		if valued, ok := wrappers[prog]; ok {
			if prog == "command" && i+1 < len(w) && (w[i+1] == "-v" || w[i+1] == "-V") {
				return Hit{}, false // looks the program up, runs nothing
			}
			i++
			for i < len(w) && (strings.HasPrefix(w[i], "-") || (prog == "env" && isAssignment(w[i]))) {
				if w[i] == "--" {
					i++
					break
				}
				if valued[w[i]] {
					i++
				}
				i++
			}
			if prog == "timeout" && i < len(w) { // its duration
				i++
			}
			continue
		}
		if prog == "eval" && depth < maxDepth {
			return check(strings.Join(w[i+1:], " "), depth+1)
		}
		if shells[prog] && depth < maxDepth {
			for j := i + 1; j < len(w); j++ {
				a := w[j]
				if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
					if j+1 < len(w) {
						return check(w[j+1], depth+1)
					}
					break
				}
				if a == "-o" || a == "+o" || a == "-O" || a == "+O" { // set option names a value
					j++
					continue
				}
				if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
					break
				}
			}
			return Hit{}, false
		}
		if prog != "bd" {
			return Hit{}, false
		}
		return checkBD(w[i+1:])
	}
	return Hit{}, false
}

// checkBD reads bd's arguments: global flags, then the subcommand. A call
// asking for help runs nothing and is never a hit.
func checkBD(args []string) (Hit, bool) {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--help" || a == "-h" {
			return Hit{}, false
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if bdValued[a] {
				i++
			}
			continue
		}
		switch a {
		case "reclaim":
			return reclaim, true
		case "update":
			if hasClaim(args[i+1:]) {
				return updateClaim, true
			}
		case "ready":
			if hasClaim(args[i+1:]) {
				return readyClaim, true
			}
		}
		return Hit{}, false
	}
	return Hit{}, false
}

// hasClaim reports a --claim flag that is not explicitly false.
func hasClaim(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--claim" {
			return true
		}
		if v, ok := strings.CutPrefix(a, "--claim="); ok {
			switch strings.ToLower(v) {
			case "false", "0", "f":
				continue
			}
			return true
		}
	}
	return false
}

func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && (i == 0 || !(r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func isRedirect(w string) bool {
	t := strings.TrimLeft(w, "0123456789")
	return strings.HasPrefix(t, "<") || strings.HasPrefix(t, ">") || strings.HasPrefix(t, "&>")
}

// redirectNeedsTarget reports a redirect operator with its target in the
// next word: `>`, `2>>` or `>|`, but not `>out`, `2>&1` or `>&-`.
func redirectNeedsTarget(w string) bool {
	t := strings.TrimLeft(w, "0123456789")
	t = strings.TrimLeft(t, "&<>|")
	return t == ""
}

// simpleCommands splits s into the words of each simple command. Quotes
// group words; an unquoted ; & | ( ) ` or newline ends a command; a heredoc
// body is skipped, and a << inside (( )) is a shift. A command substitution
// inside double quotes is read by the same rules up to its closing ) or `,
// and its commands are appended.
func simpleCommands(s string) [][]string {
	cmds, _ := scan([]rune(s), 0, 0, 0)
	return cmds
}

// maxNest bounds command substitutions nested inside double quotes; past it
// a substitution is read as quoted text.
const maxNest = 16

// scan reads r from start. With stop set to ')' or '`', it returns at the
// unquoted stop rune that closes a substitution opened inside double quotes,
// with the index of that rune; otherwise it reads to the end and returns
// len(r).
func scan(r []rune, start int, stop rune, nest int) ([][]string, int) {
	var (
		cmds     [][]string
		words    []string
		cur      strings.Builder
		inWord   bool
		raw      rune      // the last rune written unquoted, 0 after quoted text
		heredocs []heredoc // pending: their bodies start at the next newline
		parens   []bool    // open parentheses, true for an arithmetic (( ... ))
		arith    int       // how many of parens are arithmetic
	)
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
		raw = 0
	}
	endCmd := func() {
		endWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
			words = nil
		}
	}
	write := func(c rune) {
		cur.WriteRune(c)
		inWord = true
		raw = c
	}
	isHeredoc := func(i int) bool { // << but not <<<
		return r[i] == '<' && i+1 < len(r) && r[i+1] == '<' && (i+2 >= len(r) || r[i+2] != '<')
	}
	for i := start; i < len(r); i++ {
		c := r[i]
		switch {
		case stop != 0 && c == stop && (stop != ')' || len(parens) == 0):
			endCmd()
			return cmds, i
		case c == '\\' && i+1 < len(r):
			i++
			if r[i] != '\n' { // a line continuation joins lines
				write(r[i])
			}
		case c == '\'':
			inWord = true
			for i++; i < len(r) && r[i] != '\''; i++ {
				cur.WriteRune(r[i])
			}
			raw = 0
		case c == '"':
			inWord = true
			for i++; i < len(r) && r[i] != '"'; i++ {
				switch {
				case r[i] == '\\' && i+1 < len(r) && strings.ContainsRune("\"\\$`", r[i+1]):
					i++
				case nest < maxNest && r[i] == '$' && i+1 < len(r) && r[i+1] == '(' && (i+2 >= len(r) || r[i+2] != '('):
					sub, end := scan(r, i+2, ')', nest+1)
					cmds = append(cmds, sub...)
					i = end
					continue
				case nest < maxNest && r[i] == '`':
					sub, end := scan(r, i+1, '`', nest+1)
					cmds = append(cmds, sub...)
					i = end
					continue
				}
				cur.WriteRune(r[i])
			}
			raw = 0
		case c == '(':
			endCmd()
			isArith := i+1 < len(r) && r[i+1] == '('
			if isArith {
				i++
				arith++
			}
			parens = append(parens, isArith)
		case c == ')':
			endCmd()
			if n := len(parens); n > 0 {
				if parens[n-1] {
					arith--
					if i+1 < len(r) && r[i+1] == ')' {
						i++
					}
				}
				parens = parens[:n-1]
			}
		case (c == '&' || c == '|') && raw == '>',
			c == '&' && raw == '<',
			c == '&' && !inWord && i+1 < len(r) && r[i+1] == '>':
			write(c) // part of a redirect: >&2, <&0, >|, &>
		case c == '<' && i+2 < len(r) && r[i+1] == '<' && r[i+2] == '<':
			cur.WriteString("<<<") // a here-string: its word follows
			inWord = true
			raw = 0
			i += 2
		case isHeredoc(i) && arith > 0:
			cur.WriteString("<<") // a shift inside (( )), not a heredoc
			inWord = true
			raw = 0
			i++
		case isHeredoc(i):
			endWord()
			i += 2
			h := heredoc{}
			if i < len(r) && r[i] == '-' {
				h.tabs = true
				i++
			}
			for i < len(r) && (r[i] == ' ' || r[i] == '\t') {
				i++
			}
			var d strings.Builder
			for ; i < len(r) && !strings.ContainsRune(" \t\n;&|()<>", r[i]); i++ {
				if r[i] != '\'' && r[i] != '"' && r[i] != '\\' {
					d.WriteRune(r[i])
				}
			}
			i--
			if h.delim = d.String(); h.delim != "" {
				heredocs = append(heredocs, h)
			}
		case c == '\n':
			endCmd()
			pos := i + 1
			for _, h := range heredocs {
				pos = h.skip(r, pos)
			}
			i = pos - 1
			heredocs = nil
		case c == ';' || c == '&' || c == '|' || c == '`':
			endCmd()
		case c == ' ' || c == '\t':
			endWord()
		case c == '#' && !inWord:
			for i < len(r) && r[i] != '\n' {
				i++
			}
			i--
		default:
			write(c)
		}
	}
	endCmd()
	return cmds, len(r)
}

type heredoc struct {
	delim string
	tabs  bool // <<- strips leading tabs from the closing line
}

// skip returns the index just past the line that closes h's body, which
// starts at r[from], or len(r) when it never closes.
func (h heredoc) skip(r []rune, from int) int {
	i := from
	for i < len(r) {
		end := i
		for end < len(r) && r[end] != '\n' {
			end++
		}
		line := string(r[i:end])
		if h.tabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == h.delim {
			return min(end+1, len(r))
		}
		i = end + 1
	}
	return len(r)
}
