// Package daemon implements busybae's background daemon and the CLI-side
// poke/spawn logic that lets any bazelisk invocation keep the daemon alive.
//
// The design is deliberately minimal:
//
//   - The daemon listens on a Unix domain socket. Each connection is a
//     "poke" that resets the idle timer.
//   - The CLI attempts to connect first. If the connect succeeds, it sends
//     the poke and exits — this is the hot path and needs to be fast.
//   - If the connect fails (no daemon listening, or a stale socket), the
//     CLI removes the stale socket and spawns a detached child running the
//     daemon, then exits without waiting.
//   - The daemon monitors the elapsed time since its most recent poke. Once
//     it exceeds IdleTimeout, the daemon shuts down.
package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ErrAlreadyRunning is returned when a daemon is already listening on the
// configured socket path.
var ErrAlreadyRunning = errors.New("daemon already running")

// Config controls a daemon run.
type Config struct {
	// SocketPath is the Unix socket path the daemon listens on.
	SocketPath string
	// IdleTimeout is how long the daemon runs without a poke before
	// exiting. Zero disables the idle timeout (the daemon runs until its
	// context is cancelled).
	IdleTimeout time.Duration
	// GCInterval is how often the daemon runs its GC callback. Zero
	// disables scheduled GC.
	GCInterval time.Duration
	// OnGC is called every GCInterval. It should honor ctx cancellation.
	OnGC func(ctx context.Context)
	// Logger receives structured events. Nil means slog.Default().
	Logger *slog.Logger
	// Now is a clock hook for tests.
	Now func() time.Time
}

// Run starts the daemon and blocks until ctx is cancelled or the idle
// timeout fires. Only one daemon can hold a given SocketPath at a time; if a
// second daemon starts against the same path, the second call returns
// ErrAlreadyRunning.
func Run(ctx context.Context, cfg Config) error {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o700); err != nil {
		return fmt.Errorf("mkdir socket parent: %w", err)
	}

	ln, err := listen(cfg.SocketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(cfg.SocketPath)

	log.Info("daemon listening",
		slog.String("socket", cfg.SocketPath),
		slog.Duration("idle_timeout", cfg.IdleTimeout),
		slog.Duration("gc_interval", cfg.GCInterval))

	var lastPokeNanos atomic.Int64
	lastPokeNanos.Store(now().UnixNano())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	// Accept loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				if runCtx.Err() != nil {
					return
				}
				if errors.Is(err, net.ErrClosed) {
					return
				}
				log.Warn("accept failed", slog.Any("err", err))
				return
			}
			lastPokeNanos.Store(now().UnixNano())
			go handleConn(conn, log)
		}
	}()

	// Idle watchdog.
	if cfg.IdleTimeout > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Tick often enough to detect idle within ~1s of the
			// budget; cap the tick to a full minute.
			tick := max(cfg.IdleTimeout/10, time.Second)
			tick = min(tick, time.Minute)
			t := time.NewTicker(tick)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					last := time.Unix(0, lastPokeNanos.Load())
					if now().Sub(last) >= cfg.IdleTimeout {
						log.Info("idle timeout reached, shutting down",
							slog.Time("last_poke", last))
						cancel()
						_ = ln.Close()
						return
					}
				}
			}
		}()
	}

	// GC scheduler.
	if cfg.GCInterval > 0 && cfg.OnGC != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(cfg.GCInterval)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					cfg.OnGC(runCtx)
				}
			}
		}()
	}

	<-runCtx.Done()
	_ = ln.Close()
	wg.Wait()
	return nil
}

// handleConn implements the trivial poke protocol.
func handleConn(conn net.Conn, log *slog.Logger) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	switch line {
	case "PING\n":
		_, _ = conn.Write([]byte("OK\n"))
	default:
		log.Debug("unknown request", slog.String("line", line))
		_, _ = conn.Write([]byte("ERR\n"))
	}
}

// listen opens a Unix socket at path. If path already exists but no live
// daemon is answering pokes on it, it is treated as stale and removed. If a
// live daemon holds the path, listen returns ErrAlreadyRunning.
func listen(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		// A file at the socket path proves nothing on its own: a bare
		// `connect()` can succeed against a bound-but-unaccepted
		// listener leaked from a defunct process. Verify liveness by
		// completing a full poke handshake — only an OK response
		// proves a real daemon is on the other end.
		if perr := Poke(path, 500*time.Millisecond); perr == nil {
			return nil, ErrAlreadyRunning
		}
		// Nobody home — remove the stale socket.
		if rmErr := os.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("remove stale socket: %w", rmErr)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}

// Poke connects to the daemon at socketPath and sends a single poke. It
// returns nil if the daemon acknowledged, or an error (including a wrapped
// syscall.ECONNREFUSED / os.ErrNotExist when no daemon is running).
func Poke(socketPath string, timeout time.Duration) error {
	c, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte("PING\n")); err != nil {
		return err
	}
	br := bufio.NewReader(c)
	resp, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	if resp != "OK\n" {
		return fmt.Errorf("unexpected response: %q", resp)
	}
	return nil
}

// IsUnavailable reports whether err from Poke means "no daemon there" as
// opposed to a genuine protocol error.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	// Any connect error we care about is a *net.OpError wrapping the
	// underlying syscall error. The precise sentinels are OS-specific
	// (ECONNREFUSED, ENOENT), and we've already handled ErrNotExist
	// above; any other dial failure is close enough to "unavailable"
	// for the CLI's purposes.
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}
