//go:build !unix

package main

// pidAlive on non-unix platforms always reports alive (fail closed): without
// signal-0 semantics the gate cannot prove a rival is gone, so a fresh
// rival heartbeat always blocks boot.
func pidAlive(pid int) bool {
	return pid > 0
}
