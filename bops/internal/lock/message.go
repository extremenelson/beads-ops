package lock

import (
	"fmt"
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
	Run   string
	Actor string
	State string // StateHeld, StateLanding, or "" if unreadable
	Nonce string
	At    time.Time // zero if unreadable
}

// Ref is one claim as read from git.
type Ref struct {
	ID  string
	SHA string
	Message
}

func (m Message) encode() (string, error) {
	for _, v := range []string{m.Run, m.Actor, m.State, m.Nonce} {
		if strings.ContainsAny(v, "\r\n\x00") {
			return "", &Error{Code: CodeBadMessage, Err: fmt.Errorf("value %q contains a line break", v)}
		}
	}
	return fmt.Sprintf("run=%s\nactor=%s\nstate=%s\nnonce=%s\nat=%s\n", m.Run, m.Actor, m.State, m.Nonce, m.At.UTC().Format(time.RFC3339)), nil
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
		case "nonce":
			m.Nonce = v
		case "state":
			if v == StateHeld || v == StateLanding {
				m.State = v
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
