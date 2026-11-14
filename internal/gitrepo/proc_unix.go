//go:build unix

package gitrepo

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts cmd in a process group of its own and makes the
// end of its context kill the whole group, so that the transport helpers
// git starts (git-remote-https, ssh) do not outlive it.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
