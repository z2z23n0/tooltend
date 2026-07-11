//go:build darwin || linux

package kick

import (
	"os"
	"os/exec"
	"syscall"
)

func startDetached(executable string, args ...string) error {
	command := exec.Command(executable, args...)
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	command.Stdin, command.Stdout, command.Stderr = null, null, null
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
