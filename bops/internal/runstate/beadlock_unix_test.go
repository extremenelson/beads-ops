//go:build !windows

package runstate

import (
	"errors"
	"syscall"
	"testing"
)

// flock's busy answers are ErrBeadBusy, and a file system with no locks is
// ErrBeadLockUnsupported, so a command can run unserialized there and warn
// rather than fail (bops-yph).
func TestLockErrorNamesBusyAndUnsupported(t *testing.T) {
	// A slice, not a map: on Linux ENOTSUP and EOPNOTSUPP are one value.
	for _, c := range []struct{ err, want error }{
		{syscall.EWOULDBLOCK, ErrBeadBusy},
		{syscall.ENOTSUP, ErrBeadLockUnsupported},
		{syscall.EOPNOTSUPP, ErrBeadLockUnsupported},
		{syscall.ENOLCK, ErrBeadLockUnsupported},
	} {
		if got := lockError(c.err); !errors.Is(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.err, got, c.want)
		}
	}
	if got := lockError(syscall.EIO); errors.Is(got, ErrBeadBusy) || errors.Is(got, ErrBeadLockUnsupported) {
		t.Errorf("EIO must stay itself, got %v", got)
	}
}
