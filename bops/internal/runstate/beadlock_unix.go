//go:build !windows

package runstate

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on path without waiting. The lock belongs
// to the open file, so the kernel drops it when the process exits however it
// exits. A prune may unlink the file between this open and the flock; the
// path then names a new file another process can lock, so after locking it
// checks the path still names the file it holds, and starts again if not
// (bops-exb).
func lockFile(path string) (func(), error) {
	for try := 0; ; try++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		beforeFlock()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			return nil, lockError(err)
		}
		same, err := namesFile(path, f)
		if err == nil && same {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		if try == lockFileTries-1 {
			return nil, ErrBeadBusy // the file keeps being replaced: someone else is at it
		}
	}
}

// lockFileTries bounds lockFile's restarts after a prune replaced its file.
const lockFileTries = 10

// beforeFlock runs between lockFile's open and its flock. Tests set it to
// prune the file in that window; a test that does must not run in parallel.
var beforeFlock = func() {}

// pruneOpened runs between pruneLockFile's open and its flock. Tests set it
// to replace the file in that window; a test that does must not run in
// parallel.
var pruneOpened = func() {}

// namesFile reports whether path still names the open file f. A path that
// names nothing is false; a check that cannot be made is an error.
func namesFile(path string, f *os.File) (bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	pi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(fi, pi), nil
}

// pruneLockFile removes path if it can lock it at once, unlinking it while it
// holds the lock. A busy file, or one the path no longer names, is left.
func pruneLockFile(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	pruneOpened()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false, nil
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if same, err := namesFile(path, f); err != nil || !same {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
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
