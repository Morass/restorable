package run

import (
	"os/exec"
	"syscall"
	"time"
)

// groupKill makes a deadline actually end the work: the tool runs in its own
// process group, cancelling kills the whole group, and the wait gives up shortly
// afterwards. Without this a grandchild (`sh -c 'sleep 5'` inside a wrapper) keeps
// the output pipe open and the deadline has no effect until it finishes by itself.
func groupKill(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
}
