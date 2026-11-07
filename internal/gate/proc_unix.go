//go:build unix

package gate

import (
	"os/exec"
	"syscall"
)

// npx starts a shell which starts node; run them in their own process group
// so stopping Lumen stops all of them.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}
