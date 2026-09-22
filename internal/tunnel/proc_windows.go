//go:build windows

package tunnel

import (
	"os/exec"
	"syscall"
)

// hideConsoleWindow keeps QEMU from popping up its own console window when the
// server runs as a GUI/tray application.
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
