//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// configureProcGroup places the local-toolchain child in its own process group
// and makes ctx-cancel (kill control op or timeout) SIGKILL the WHOLE group,
// not just the direct child. Without this, killing sh/ansible-playbook leaves
// grandchildren (module subprocesses, sleeps, ssh muxes) alive holding the
// stdout/stderr pipes open — the log scanners then block until every orphan
// exits, so a timed-out run doesn't actually end at its deadline.
func configureProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid ⇒ the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
