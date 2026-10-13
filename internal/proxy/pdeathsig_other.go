//go:build !linux

package proxy

import "os/exec"

func setPdeathsig(*exec.Cmd) {}
