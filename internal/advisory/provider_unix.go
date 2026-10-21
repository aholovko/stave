//go:build unix

package advisory

import (
	"os"
	"os/exec"
	"syscall"
)

// Cancel the whole group so killing the interpreter does not leave its selectors running.
// This removes the backend from the terminal's foreground group, so Ctrl-C must arrive through the caller's context.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone // the group had already ended
		}
		return err
	}
}
