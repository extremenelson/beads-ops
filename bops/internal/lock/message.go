package lock

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Values of the ref's state key.
const (
	StateHeld    = "held"
	StateLanding = "landing"
)

// maxMessage bounds what is parsed from a ref anyone with push access can write.
const maxMessage = 4096

// futureTolerance is how far ahead of this clock a timestamp may be before it
// is reported. Machines are never perfectly in step.
const futureTolerance = 5 * time.Minute

// Message is the content of a lock commit, one key=value per line (D20).
type Message struct {
	Run     string
	Actor   string
	Machine string // the holder's machine; "" when unrecorded (a bops before bops-xwr)
	State   string // StateHeld, StateLanding, or "" if unreadable
	Nonce   string
	At      time.Time // zero if unreadable
	// Gen is the claim's generation: 1 for a new claim, unchanged by its own
	// run's writes, one more for each takeover (claim --force, claim
	// --resume). 0 means unrecorded: a ref written by a bops before
	// generations, or one whose predecessor could not be read.
	Gen int
}

// Ref is one claim as read from git.
type Ref struct {
	ID  string
	SHA string
	Message
}

func (m Message) encode() (string, error) {
	for _, v := range []string{m.Run, m.Actor, m.Machine, m.State, m.Nonce} {
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", &Error{Code: CodeBadMessage, Err: fmt.Errorf("value %q contains a line break", v)}
		}
	}
	machine := ""
	if m.Machine != "" { // omitted when unknown, so the line is never empty
		machine = "machine=" + m.Machine + "\n"
	}
	gen := ""
	if m.Gen > 0 { // omitted when unrecorded, as machine is
		gen = "gen=" + strconv.Itoa(m.Gen) + "\n"
	}
	return fmt.Sprintf("run=%s\nactor=%s\n%sstate=%s\n%snonce=%s\nat=%s\n", m.Run, m.Actor, machine, m.State, gen, m.Nonce, m.At.UTC().Format(time.RFC3339)), nil
}

// ParseMessage reads untrusted text and never fails. Unknown keys are ignored,
// the first value for a key wins, and anything unreadable stays empty. Callers
// must treat an empty state or a zero time as unknown, which is never stale.
func ParseMessage(s string) Message {
	var m Message
	if len(s) > maxMessage {
		return m
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok || seen[k] {
			continue
		}
		seen[k] = true
		switch k {
		case "run":
			m.Run = v
		case "actor":
			m.Actor = v
		case "machine":
			m.Machine = v
		case "nonce":
			m.Nonce = v
		case "state":
			if v == StateHeld || v == StateLanding {
				m.State = v
			}
		case "gen":
			if n, err := strconv.Atoi(v); err == nil && n > 0 && v == strconv.Itoa(n) {
				m.Gen = n
			}
		case "at":
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				m.At = t.UTC()
			}
		}
	}
	return m
}

// Stale reports whether the claim is older than window. A claim with an
// unknown or future time is never stale.
func (m Message) Stale(now time.Time, window time.Duration) bool {
	return !m.At.IsZero() && !m.At.After(now) && now.Sub(m.At) > window
}

// Anomalous reports a timestamp too far ahead of this machine's clock.
func (m Message) Anomalous(now time.Time) bool {
	return m.At.After(now.Add(futureTolerance))
}
