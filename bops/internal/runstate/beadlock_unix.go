//go:build !windows

package runstate

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on path without waiting. The lock belongs
// to the open file, so the kernel drops it when the process exits however it
// exits. The file is never removed: unlinking a lock file another process has
// open would let a third lock a fresh one beside it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, lockError(err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// lockError names flock's two expected failures: another holder, and a file
// system with no locks.
func lockError(err error) error {
	switch {
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return ErrBeadBusy
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOLCK):
		return ErrBeadLockUnsupported
	}
	return err
}
