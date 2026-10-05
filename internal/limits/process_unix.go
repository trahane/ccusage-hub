//go:build unix

package limits

import (
	"os"
	"os/exec"
	"syscall"
)

// CodexBar launches provider CLIs and shells. Cancel the whole process group,
// otherwise each timed-out poll can leave shells and their children running.
func containCommand(cmd *exec.Cmd) func() {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = kill
	return func() { _ = kill() }
}
