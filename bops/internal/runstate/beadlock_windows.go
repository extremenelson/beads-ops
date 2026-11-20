//go:build windows

package runstate

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2

	errorInvalidFunction syscall.Errno = 1
	errorNotSupported    syscall.Errno = 50
	errorLockViolation   syscall.Errno = 33
)

// lockFile takes an exclusive LockFileEx lock on path without waiting. The
// handle is opened shared, so a virus scanner or indexer that opens the file
// does not look like another bops process; only the lock excludes. Windows
// releases it when the handle closes, which it does when the process exits
// however it exits.
func lockFile(path string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	share := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE)
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, share, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	var ol syscall.Overlapped
	r, _, callErr := procLockFileEx.Call(uintptr(h), lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		_ = syscall.CloseHandle(h)
		return nil, lockError(callErr)
	}
	return func() {
		// Unlock before closing: Windows frees a lock at close only "at some
		// point", and sync and sweep do not wait for a busy bead.
		var un syscall.Overlapped
		_, _, _ = procUnlockFileEx.Call(uintptr(h), 0, 1, 0, uintptr(unsafe.Pointer(&un)))
		_ = syscall.CloseHandle(h)
	}, nil
}

func lockError(err error) error {
	switch {
	case errors.Is(err, errorLockViolation):
		return ErrBeadBusy
	case errors.Is(err, errorNotSupported), errors.Is(err, errorInvalidFunction):
		return ErrBeadLockUnsupported
	}
	return err
}

// pruneLockFile removes nothing on Windows. A file deleted while another
// handle has it open lingers as delete-pending, and on older Windows its
// name cannot be reopened until every handle closes, so a prune could make a
// bead unlockable for as long as some process holds it; the files are empty
// (bops-exb).
func pruneLockFile(string) (bool, error) { return false, nil }
