//go:build !windows

package codespace

import "os/exec"

func hideConsoleWindow(*exec.Cmd) {}
