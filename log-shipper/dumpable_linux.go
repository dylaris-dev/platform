package main

import "syscall"

const prSetDumpable = 4 // PR_SET_DUMPABLE, linux/prctl.h

// hideFromSameUID makes /proc/<pid>/environ, /proc/<pid>/mem and ptrace of
// this process root-only. The flag is per process and is reset by execve, so
// the server process this one starts stays an ordinary, debuggable JVM.
func hideFromSameUID() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
