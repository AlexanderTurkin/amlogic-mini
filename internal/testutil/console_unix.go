//go:build !windows

package testutil

import "syscall"

func consoleProcessAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true}
}
