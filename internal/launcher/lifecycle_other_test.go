//go:build !linux

package launcher

import (
	"os"
	"os/exec"
	"testing"
)

// Off Linux there is no PDEATHSIG. SetDeathSig may leave SysProcAttr nil or
// initialise an empty one, but the command must still run afterwards.
func TestSetDeathSig_OffLinuxKeepsCommandUsable(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")

	SetDeathSig(cmd)

	if err := cmd.Run(); err != nil {
		t.Fatalf("command not runnable after SetDeathSig: %v", err)
	}
}
