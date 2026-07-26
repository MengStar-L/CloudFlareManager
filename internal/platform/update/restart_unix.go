//go:build !windows

package update

import (
	"os"
	"os/exec"
	"syscall"
)

// Restart replaces the current process with the (freshly installed)
// executable. Under systemd with Restart=always, exiting would also work;
// exec keeps the same PID and works without a supervisor too.
func (u *Updater) Restart() {
	exe, err := u.executable()
	if err != nil {
		os.Exit(0)
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		// exec 失败时退回“启动新进程后退出”，交给进程管理器接管。
		command := exec.Command(exe, os.Args[1:]...)
		command.Stdout, command.Stderr, command.Stdin = os.Stdout, os.Stderr, os.Stdin
		_ = command.Start()
		os.Exit(0)
	}
}
