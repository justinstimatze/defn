//go:build !windows

package main

import "syscall"

// detachedSysProcAttr puts the spawned `defn serve` into its own
// session and process group so it survives the parent (cmdRestart)
// exiting and isn't reaped on terminal hangup.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}
