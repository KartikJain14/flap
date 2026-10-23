package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary act as flapd when FLAPD_TEST_MAIN is set, so
// the CLI can be exercised as real processes (signals, exit codes, output).
func TestMain(m *testing.M) {
	if os.Getenv("FLAPD_TEST_MAIN") == "1" {
		os.Args = append([]string{"flapd"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func flapd(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "FLAPD_TEST_MAIN=1")
	return cmd
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := flapd(args...).CombinedOutput()
	return string(out), err
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeConfig(t *testing.T, dir string, port int) string {
	t.Helper()
	cfg := fmt.Sprintf(`[server]
listen = "127.0.0.1:%d"
host_key_files = [%q]
identity_secret_file = %q
shutdown_grace = "2s"
[storage]
path = %q
[logging]
format = "text"
stats_interval = "0s"
`, port, filepath.Join(dir, "host_key"), filepath.Join(dir, "secret"), filepath.Join(dir, "flap.db"))
	p := filepath.Join(dir, "flapd.toml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCLILifecycle(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	cfg := writeConfig(t, dir, port)

	if out, err := run(t, "check-config", "-config", cfg, "-listen"); err != nil || strings.TrimSpace(out) != fmt.Sprintf("127.0.0.1:%d", port) {
		t.Fatalf("check-config -listen: %q %v", out, err)
	}
	out, err := run(t, "init", "-config", cfg)
	if err != nil || !strings.Contains(out, "created host key") || !strings.Contains(out, "created identity secret") {
		t.Fatalf("init: %q %v", out, err)
	}
	again, err := run(t, "init", "-config", cfg)
	if err != nil || strings.Contains(again, "created") {
		t.Fatalf("init is not idempotent: %q %v", again, err)
	}
	fp, err := run(t, "fingerprint", "-config", cfg)
	if err != nil || !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("fingerprint: %q %v", fp, err)
	}

	// A broken configuration is rejected with a useful message.
	bad := filepath.Join(dir, "bad.toml")
	os.WriteFile(bad, []byte("[limits]\nmax_sessions = 0\n"), 0o600)
	if out, err := run(t, "check-config", "-config", bad); err == nil || !strings.Contains(out, "limits.max_sessions") {
		t.Fatalf("bad config accepted: %q", out)
	}

	// Serve, health-check, back up, then stop with SIGTERM.
	srv := flapd("serve", "-config", cfg)
	var logs bytes.Buffer
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Process.Kill()
	healthy := false
	for i := 0; i < 100 && !healthy; i++ {
		if _, err := run(t, "healthcheck", "-config", cfg); err == nil {
			healthy = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("server never became healthy:\n%s", logs.String())
	}

	// A second server on the same port fails clearly.
	out, err = run(t, "serve", "-config", cfg)
	if err == nil || !strings.Contains(out, "already in use") {
		t.Fatalf("port conflict not reported: %q %v", out, err)
	}

	backup := filepath.Join(dir, "backup.db")
	if out, err := run(t, "backup", "-config", cfg, "-out", backup); err != nil || !strings.Contains(out, "wrote") {
		t.Fatalf("backup: %q %v", out, err)
	}
	if st, err := os.Stat(backup); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("backup file: %v %v", st, err)
	}
	if out, err := run(t, "check-db", "-config", cfg, "-full"); err != nil || !strings.Contains(out, "OK") {
		t.Fatalf("check-db: %q %v", out, err)
	}
	if out, err := run(t, "remove-player", "-config", cfg, "-name", "Nobody"); err == nil || !strings.Contains(out, "no player") {
		t.Fatalf("remove-player of unknown name: %q %v", out, err)
	}

	srv.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- srv.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve exited with %v:\n%s", err, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not stop on SIGTERM:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "msg=stopped") {
		t.Fatalf("no clean shutdown in logs:\n%s", logs.String())
	}
}

func TestCLIUsageErrors(t *testing.T) {
	if out, err := run(t, "no-such-command"); err == nil || !strings.Contains(out, "unknown command") {
		t.Fatalf("unknown command: %q %v", out, err)
	}
	if out, err := run(t, "version"); err != nil || !strings.HasPrefix(out, "flapd ") {
		t.Fatalf("version: %q %v", out, err)
	}
	if out, err := run(t, "serve", "-config", "/nonexistent/flapd.toml"); err == nil || !strings.Contains(out, "read config") {
		t.Fatalf("missing config: %q %v", out, err)
	}
}
