//go:build !windows

package mcp

import "syscall"

// detachedSysProcAttr configures a spawned defn serve replacement to
// survive this process's own exit -- own session, detached from the
// controlling terminal/parent process group. Mirrors cmd/defn's
// identical restart_unix.go helper (can't import a main package, so
// this is a deliberate small duplication rather than a shared export).
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}
