//go:build !windows

package tunnel

import "os/exec"

func hideConsoleWindow(*exec.Cmd) {}
