//go:build windows

package main

import "syscall"

// detachedSysProcAttr is a no-op on Windows (no Setsid / process
// groups in the same form). Detachment is handled by the lack of a
// Wait call after Start.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
