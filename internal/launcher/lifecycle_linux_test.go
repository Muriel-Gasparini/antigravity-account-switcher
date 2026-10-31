//go:build linux

package launcher

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestSetDeathSig(t *testing.T) {
	cmd := exec.Command("true")
	SetDeathSig(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr to be non-nil after SetDeathSig")
	}
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Errorf("expected Pdeathsig SIGTERM, got %v", cmd.SysProcAttr.Pdeathsig)
	}
}

func TestSetDeathSig_PreservesExistingSysProcAttr(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	SetDeathSig(cmd)
	if !cmd.SysProcAttr.Setpgid {
		t.Error("expected existing SysProcAttr fields to be preserved")
	}
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Errorf("expected Pdeathsig SIGTERM, got %v", cmd.SysProcAttr.Pdeathsig)
	}
}
