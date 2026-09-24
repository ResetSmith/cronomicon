//go:build windows

package agent

import "os/exec"

// configureProcGroup is a no-op on Windows: exec.CommandContext's default
// kill (the direct process) is what's available; cmd.WaitDelay (set at the
// call site) still unblocks the log scanners if a grandchild inherits the
// output pipes.
func configureProcGroup(cmd *exec.Cmd) {}
