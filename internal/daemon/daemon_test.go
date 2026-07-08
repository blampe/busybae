package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tempSocket(t *testing.T) string {
	t.Helper()
	// Unix domain socket paths cap at ~104 bytes on macOS and ~108 on
	// Linux. Bazel's TEST_TMPDIR is deep enough (sandbox execroot +
	// testlogs + test name) that t.TempDir() commonly overflows on
	// darwin. Anchor under /tmp so paths stay short across `go test`
	// AND `bazel test`.
	dir, err := os.MkdirTemp("/tmp", "bb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func TestPokeUpdatesIdleTimer(t *testing.T) {
	sock := tempSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemonErr := make(chan error, 1)
	go func() {
		daemonErr <- Run(ctx, Config{
			SocketPath:  sock,
			IdleTimeout: 500 * time.Millisecond,
		})
	}()

	// Wait for the socket to appear before poking.
	waitForSocket(t, sock)

	// Poke repeatedly to keep it alive for ~1s, then stop poking and
	// verify it shuts down within its idle window.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if err := Poke(sock, time.Second); err != nil {
			t.Fatalf("poke: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	select {
	case err := <-daemonErr:
		t.Fatalf("daemon exited early: %v", err)
	default:
	}

	// Stop poking. The daemon should idle-out within ~1.5s.
	select {
	case err := <-daemonErr:
		if err != nil {
			t.Fatalf("daemon returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("daemon did not idle-out")
	}
}

func TestListenRejectsSecondDaemon(t *testing.T) {
	sock := tempSocket(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Run(ctx, Config{SocketPath: sock, IdleTimeout: 5 * time.Second})
	waitForSocket(t, sock)

	err := Run(ctx, Config{SocketPath: sock, IdleTimeout: 5 * time.Second})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("want ErrAlreadyRunning, got %v", err)
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	// Create a stale socket file (no listener behind it).
	sock := tempSocket(t)
	f, err := createStaleFile(sock)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Config{SocketPath: sock, IdleTimeout: 5 * time.Second}) }()

	waitForSocket(t, sock)
	if err := Poke(sock, time.Second); err != nil {
		t.Fatalf("poke: %v", err)
	}
	cancel()
	<-done
}

func TestGCSchedulerFires(t *testing.T) {
	sock := tempSocket(t)
	var calls atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer wg.Done()
		_ = Run(ctx, Config{
			SocketPath:  sock,
			IdleTimeout: 5 * time.Second,
			GCInterval:  50 * time.Millisecond,
			OnGC:        func(context.Context) { calls.Add(1) },
		})
	}()
	waitForSocket(t, sock)
	time.Sleep(250 * time.Millisecond)
	if calls.Load() < 2 {
		t.Fatalf("expected multiple GC calls, got %d", calls.Load())
	}
	cancel()
	wg.Wait()
}

func TestPokeNoDaemon(t *testing.T) {
	sock := tempSocket(t)
	err := Poke(sock, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected error poking nonexistent daemon")
	}
	if !IsUnavailable(err) {
		t.Fatalf("expected IsUnavailable=true, got %v", err)
	}
}

// waitForSocket blocks until the Unix socket at path accepts a connection
// or the test's short retry budget is exhausted.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := Poke(path, 100*time.Millisecond); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("socket %q never came up", path)
}
