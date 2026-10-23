package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"golang.org/x/crypto/ssh"

	"flap/internal/config"
	"flap/internal/store"
)

type testServer struct {
	srv     *Server
	addr    string
	st      *store.Store
	dir     string
	hostKey ssh.Signer
	cfg     config.Config
	secret  []byte
	done    chan error
	stopFn  func()
}

func testConfig(dir string) config.Config {
	cfg := config.Default()
	cfg.Server.Listen = "127.0.0.1:2222" // the test passes its own listener
	cfg.Server.HostKeyFiles = []string{filepath.Join(dir, "host_key")}
	cfg.Server.IdentitySecretFile = filepath.Join(dir, "secret")
	cfg.Server.ShutdownGrace = config.D(3 * time.Second)
	cfg.Storage.Path = filepath.Join(dir, "flap.db")
	cfg.Logging.StatsInterval = config.D(0)
	// Loopback tests make many connections from one address.
	cfg.Limits.ConnectionRatePerIP = 6000
	cfg.Limits.ConnectionBurstPerIP = 1000
	cfg.Limits.ConnectionRateGlobal = 10000
	cfg.Limits.ConnectionBurstGlobal = 1000
	cfg.Limits.MaxConnectionsPerIP = 100
	return cfg
}

func testLogger(t *testing.T) *slog.Logger {
	if os.Getenv("FLAP_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

// startServer runs a server on a loopback port. dir may be reused to
// simulate a restart with the same keys and database.
func startServer(t *testing.T, dir string, mutate func(*config.Config)) *testServer {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	cfg := testConfig(dir)
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	keys, _, err := LoadHostKeys(cfg.Server.HostKeyFiles, true)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := LoadSecret(cfg.Server.IdentitySecretFile)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), cfg.Storage.Path, store.Options{IntegrityCheck: "quick", GuestLeaderboard: cfg.Auth.GuestLeaderboard})
	if err != nil {
		t.Fatal(err)
	}
	var allow *Allowlist
	if cfg.Auth.Mode == config.AuthAllowlist {
		if allow, err = LoadAllowlist(cfg.Auth.AuthorizedKeysFile); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := New(Options{Config: cfg, Store: st, HostKeys: keys, Secret: secret, Allowlist: allow, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{srv: srv, addr: ln.Addr().String(), st: st, dir: dir, hostKey: keys[0], cfg: cfg, secret: secret, done: make(chan error, 1)}
	go func() { ts.done <- srv.Serve(ln) }()
	var once sync.Once
	ts.stopFn = func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				t.Errorf("shutdown: %v", err)
			}
			if err := <-ts.done; err != nil {
				t.Errorf("serve: %v", err)
			}
			st.Close()
		})
	}
	t.Cleanup(ts.stop)
	return ts
}

func (ts *testServer) stop() { ts.stopFn() }

func newKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// dial connects with the given key, or as a keyless (guest) client when key
// is nil.
func (ts *testServer) dial(key ssh.Signer) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            "flappy",
		HostKeyCallback: ssh.FixedHostKey(ts.hostKey.PublicKey()),
		Timeout:         5 * time.Second,
	}
	if key != nil {
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(key)}
	} else {
		cfg.Auth = []ssh.AuthMethod{ssh.KeyboardInteractive(func(name, instruction string, q []string, echo []bool) ([]string, error) {
			return make([]string, len(q)), nil
		})}
	}
	return ssh.Dial("tcp", ts.addr, cfg)
}

// term is a client terminal: an emulator fed by the session's output whose
// replies (to terminal queries) are sent back like a real terminal would.
type term struct {
	t      *testing.T
	client *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser
	mu     sync.Mutex
	emu    *vt.Emulator
	bytes  atomic.Int64
	ended  chan error
	inMu   sync.Mutex
	raw    []byte // first bytes of output, for debugging
}

func (ts *testServer) open(t *testing.T, key ssh.Signer, w, h int) *term {
	t.Helper()
	c, err := ts.dial(key)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return openTerm(t, c, w, h, true)
}

func openTerm(t *testing.T, c *ssh.Client, w, h int, pty bool) *term {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	tm := &term{t: t, client: c, sess: sess, emu: vt.NewEmulator(w, h), ended: make(chan error, 1)}
	if pty {
		if err := sess.RequestPty("xterm-256color", h, w, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
			t.Fatalf("pty: %v", err)
		}
	}
	tm.stdin, err = sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	// Terminal replies.
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := tm.emu.Read(buf)
			if n > 0 {
				tm.inMu.Lock()
				_, _ = tm.stdin.Write(buf[:n])
				tm.inMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				tm.bytes.Add(int64(n))
				tm.mu.Lock()
				if len(tm.raw) < 1<<20 {
					tm.raw = append(tm.raw, buf[:n]...)
				}
				_, _ = tm.emu.Write(buf[:n])
				tm.mu.Unlock()
			}
			if err != nil {
				tm.ended <- sess.Wait()
				tm.closeReplies()
				return
			}
		}
	}()
	t.Cleanup(func() {
		sess.Close()
		c.Close()
		tm.closeReplies()
	})
	return tm
}

// closeReplies ends the goroutine relaying terminal replies. The emulator's
// own Close is not safe to call concurrently with Read, so the reply pipe is
// closed directly.
func (tm *term) closeReplies() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if pw, ok := tm.emu.InputPipe().(*io.PipeWriter); ok {
		_ = pw.Close()
	}
}

func (tm *term) screen() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.emu.String()
}

func (tm *term) altScreen() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.emu.IsAltScreen()
}

func (tm *term) send(s string) {
	tm.inMu.Lock()
	defer tm.inMu.Unlock()
	if _, err := tm.stdin.Write([]byte(s)); err != nil {
		tm.t.Logf("send %q: %v", s, err)
	}
}

func (tm *term) resize(w, h int) {
	tm.mu.Lock()
	tm.emu.Resize(w, h)
	tm.mu.Unlock()
	if err := tm.sess.WindowChange(h, w); err != nil {
		tm.t.Fatalf("window change: %v", err)
	}
}

// waitFor polls the screen until it contains every substring.
func (tm *term) waitFor(timeout time.Duration, subs ...string) string {
	tm.t.Helper()
	deadline := time.Now().Add(timeout)
	var s string
	for time.Now().Before(deadline) {
		s = tm.screen()
		ok := true
		for _, sub := range subs {
			if !strings.Contains(s, sub) {
				ok = false
				break
			}
		}
		if ok {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	tm.t.Fatalf("timed out waiting for %q; screen:\n%s", subs, s)
	return s
}

// waitEnd waits for the remote side to finish and returns the exit error.
func (tm *term) waitEnd(timeout time.Duration) error {
	tm.t.Helper()
	select {
	case err := <-tm.ended:
		return err
	case <-time.After(timeout):
		tm.t.Fatalf("session did not end; screen:\n%s", tm.screen())
		return nil
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus()
	}
	return -1
}

// waitUntil polls cond.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
