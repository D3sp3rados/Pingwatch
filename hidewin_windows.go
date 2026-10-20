//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow verhindert, dass tracert ein Konsolenfenster öffnet.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
