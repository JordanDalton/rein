//go:build !windows

package main

import (
	"io"
	"os/exec"
	"syscall"
)

func launchGatewayProcess(binary string, args []string, workDir string, log io.Writer) error {
	command := exec.Command(binary, args...)
	command.Dir = workDir
	command.Stdin = nil
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
