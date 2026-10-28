//go:build !windows

package server

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive flock on f without waiting. The lock dies with
// the process.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
