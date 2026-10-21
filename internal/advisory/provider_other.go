//go:build !unix

package advisory

import "os/exec"

// Without process groups, CommandContext cancels only the direct child. WaitDelay bounds any remaining pipe wait.
func isolate(*exec.Cmd) {}
