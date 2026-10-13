package proxy

import (
	"os/exec"
	"syscall"
)

// setPdeathsig has the kernel kill the child if MMP itself dies, so a crash
// cannot leave orphaned ffmpeg processes behind.
func setPdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
