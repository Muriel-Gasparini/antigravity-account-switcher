//go:build !linux

package launcher

import (
	"os/exec"
	"syscall"
)

// SetDeathSig ensures SysProcAttr is initialized on non-Linux platforms where PDEATHSIG is unavailable.
func SetDeathSig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
}
