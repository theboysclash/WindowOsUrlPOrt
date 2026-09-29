//go:build windows

package codespace

import (
	"os/exec"
	"syscall"
)

// hideConsoleWindow keeps gh and the ssh it spawns from opening console
// windows when the server runs as a GUI/tray application.
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
