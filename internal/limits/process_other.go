//go:build !unix

package limits

import "os/exec"

func containCommand(cmd *exec.Cmd) func() { return func() {} }
