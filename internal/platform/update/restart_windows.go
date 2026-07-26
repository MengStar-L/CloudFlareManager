//go:build windows

package update

import (
	"os"
	"os/exec"
)

// Restart starts the new executable as a detached process and exits.
// Windows has no exec(2); the old process must terminate so the new one can
// take over the listeners.
func (u *Updater) Restart() {
	exe, err := u.executable()
	if err == nil {
		command := exec.Command(exe, os.Args[1:]...)
		command.Stdout, command.Stderr, command.Stdin = os.Stdout, os.Stderr, os.Stdin
		_ = command.Start()
	}
	os.Exit(0)
}
