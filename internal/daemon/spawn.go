package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// SpawnDetached launches the busybae binary as a background daemon and
// returns immediately. The parent must not Wait on the child; the child is
// intentionally reparented via setsid so it survives the parent's exit.
//
// exe is the path to the busybae binary (typically os.Executable). args is
// the full argv the child should see, starting with the subcommand
// (e.g. []string{"daemon", "--socket", "/tmp/x.sock", ...}).
//
// logPath, if non-empty, receives stdout+stderr from the child. Otherwise
// they are redirected to /dev/null.
func SpawnDetached(exe string, args []string, logPath string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Detach stdio. The child must not share the parent's terminal, or
	// closing the terminal will send it SIGHUP.
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	cmd.Stdin = devnull

	if logPath != "" {
		logf, lerr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if lerr != nil {
			// Fall back to /dev/null; a missing log is not fatal.
			cmd.Stdout, cmd.Stderr = devnull, devnull
		} else {
			cmd.Stdout, cmd.Stderr = logf, logf
		}
	} else {
		cmd.Stdout, cmd.Stderr = devnull, devnull
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	// Release the child so we don't leave a zombie. Since we called
	// Setsid, the child is in its own session/process group.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}
