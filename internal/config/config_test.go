package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSampleConfigMatchesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "deploy", "flapd.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := Default(); !reflect.DeepEqual(cfg, want) {
		t.Fatalf("deploy/flapd.toml drifted from Default():\n got %+v\nwant %+v", cfg, want)
	}
}

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flapd.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingKeysKeepSecureDefaults(t *testing.T) {
	cfg, err := Load(write(t, "[server]\nlisten = \"127.0.0.1:2323\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if cfg.Server.Listen != "127.0.0.1:2323" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Limits != d.Limits || cfg.Auth != d.Auth {
		t.Fatal("limits or auth changed although they were not configured")
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	_, err := Load(write(t, "[limits]\nmax_sesions = 5\n"))
	if err == nil || !strings.Contains(err.Error(), "max_sesions") {
		t.Fatalf("expected unknown key error, got %v", err)
	}
}

func TestStagesReplaceDefaults(t *testing.T) {
	cfg, err := Load(write(t, `
[[game.stages]]
min_score = 0
speed = 0.5
gap = 0.4
spacing = 1.0
gap_shift = 0.2
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Game.Stages) != 1 || cfg.Game.Stages[0].Gap != 0.4 {
		t.Fatalf("stages = %+v", cfg.Game.Stages)
	}
}

func TestValidationErrorsAreActionable(t *testing.T) {
	body := `
[server]
listen = "nonsense"
host_key_files = []
[auth]
mode = "allowlist"
[limits]
max_sessions = 0
max_connections = 10
max_unauthenticated = 50
idle_timeout = "1s"
write_timeout = "forever"
`
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("expected error")
	}
	// The bad duration fails decoding before validation.
	if !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("decode error not reported: %v", err)
	}
	body = strings.Replace(body, `write_timeout = "forever"`, "", 1)
	_, err = Load(write(t, body))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{
		"server.listen", "server.host_key_files", "auth.authorized_keys_file",
		"limits.max_sessions = 0", "limits.max_unauthenticated (50)", "limits.idle_timeout",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestSecurityControlsCannotBeDisabled(t *testing.T) {
	cases := map[string]func(*Config){
		"zero sessions":       func(c *Config) { c.Limits.MaxSessions = 0 },
		"no keepalive":        func(c *Config) { c.Limits.KeepaliveInterval = D(0) },
		"no handshake limit":  func(c *Config) { c.Limits.HandshakeTimeout = D(0) },
		"unbounded input":     func(c *Config) { c.Limits.InputBytesPerSecond = 0 },
		"no write timeout":    func(c *Config) { c.Limits.WriteTimeout = D(0) },
		"huge terminal":       func(c *Config) { c.Limits.MaxTerminalWidth = 100000 },
		"unknown auth":        func(c *Config) { c.Auth.Mode = "none" },
		"empty auth":          func(c *Config) { c.Auth.Mode = "" },
		"no host key":         func(c *Config) { c.Server.HostKeyFiles = nil },
		"control chars motd":  func(c *Config) { c.Server.Name = "\x1b[2J" },
		"impossible game":     func(c *Config) { c.Game.Stages[0].Gap = 0.01 },
		"bad ip mode":         func(c *Config) { c.Logging.IPAddresses = "everything" },
		"tiny min size":       func(c *Config) { c.UI.MinWidth = 10 },
		"menu faster than ui": func(c *Config) { c.UI.MenuFPS = 60; c.UI.RenderFPS = 30 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestDurationText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("1m30s")); err != nil || d.Duration != 90*time.Second {
		t.Fatalf("got %v, %v", d, err)
	}
	if b, _ := d.MarshalText(); string(b) != "1m30s" {
		t.Fatalf("marshal %q", b)
	}
}

func TestMaxSessionDurationZeroDisables(t *testing.T) {
	c := Default()
	c.Limits.MaxSessionDuration = D(0)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
