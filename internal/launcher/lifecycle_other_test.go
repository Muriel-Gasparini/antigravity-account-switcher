//go:build !linux

package launcher

import (
	"os"
	"os/exec"
	"testing"
)

// SetDeathSig is a no-op off Linux: it must not touch the command and the
// command must still run afterwards.
func TestSetDeathSig_NoOpOffLinux(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	before := cmd.SysProcAttr

	SetDeathSig(cmd)

	if cmd.SysProcAttr != before {
		t.Errorf("expected SysProcAttr to be left untouched, got %v", cmd.SysProcAttr)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("command not runnable after SetDeathSig: %v", err)
	}
}
