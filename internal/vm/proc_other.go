//go:build !windows

package vm

import "os/exec"

func hideConsoleWindow(*exec.Cmd) {}
