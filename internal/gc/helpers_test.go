package gc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// lockHelperEnv, when set, tells TestMain to take an exclusive fcntl lock
// on the referenced path, print "OK\n" on stdout, and block on stdin.
// Tests that want to prove "another process holds the lock" behavior spawn
// the test binary itself with this env var — POSIX advisory locks are
// per-process, so a same-process tryLockExclusive against a lock we
// already hold would succeed silently.
const lockHelperEnv = "BUSYBAE_TEST_LOCK_HELPER_PATH"

// TestMain reroutes to the lock helper when the env var is set; otherwise
// it delegates to the standard test runner.
func TestMain(m *testing.M) {
	if path := os.Getenv(lockHelperEnv); path != "" {
		unlock, err := tryLockExclusive(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer unlock()
		fmt.Println("OK")
		// Block until parent closes stdin.
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// withHeldLock spawns a helper subprocess that acquires an exclusive
// POSIX lock on path and holds it for the duration of fn. The helper is
// signalled to exit by closing its stdin.
func withHeldLock(t *testing.T, path string, fn func()) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	// Wait for the helper's "OK" so we know the lock is held before fn
	// runs.
	rd := bufio.NewReader(stdout)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read helper ack: %v", err)
	}
	if line != "OK\n" {
		t.Fatalf("unexpected helper ack %q", line)
	}
	fn()
}

// writeFileAt creates path with the given contents and mtime, creating
// parent directories as needed.
func writeFileAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
