//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package artifacts

import (
	"errors"
	"os"
	"syscall"
)

func tryExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLockBusy
	}
	return err
}
func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
