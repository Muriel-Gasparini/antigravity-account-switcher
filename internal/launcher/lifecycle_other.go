//go:build !linux

package launcher

import "os/exec"

// SetDeathSig is a no-op on non-Linux platforms where PDEATHSIG is unavailable.
func SetDeathSig(cmd *exec.Cmd) {
	// Pdeathsig is a Linux-specific feature.
}
