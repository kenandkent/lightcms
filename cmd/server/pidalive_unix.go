//go:build unix

package main

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// pidAlive reports whether pid names a live process: signal-0 succeeds, or
// fails with EPERM (alive, different owner). ESRCH — or Go's
// "process already finished" for a reaped child of this process — means
// gone. Anything else fails closed (treated as alive so the gate rejects).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		// Cannot even reference the process: fail closed (treat as alive
		// so the gate rejects rather than risk split-brain).
		return true
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) ||
		strings.Contains(err.Error(), "already finished") {
		return false
	}
	return true
}
