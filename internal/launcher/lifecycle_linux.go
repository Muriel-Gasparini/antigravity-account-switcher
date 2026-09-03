//go:build linux

package launcher

import (
	"os/exec"
	"syscall"
)

// SetDeathSig configures the child process to receive SIGTERM when the parent process dies.
func SetDeathSig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
