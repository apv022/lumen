//go:build !unix

package gate

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
