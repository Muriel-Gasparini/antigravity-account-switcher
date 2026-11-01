package launcher

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// SetDeathSig must keep attributes the caller already set on SysProcAttr.
func TestSetDeathSig_DarwinPreservesExistingAttrs(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	SetDeathSig(cmd)

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("expected existing SysProcAttr fields to survive, got %+v", cmd.SysProcAttr)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("command not runnable after SetDeathSig: %v", err)
	}
}
