//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive lock on path, waiting up to wait for whoever
// holds it. The lock goes with the process, so a crash cannot leave it
// behind.
func lockFile(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, errBusy
		}
		time.Sleep(100 * time.Millisecond)
	}
}
