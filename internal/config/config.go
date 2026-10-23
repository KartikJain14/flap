// Package config loads and validates the flapd configuration.
//
// Every setting has a safe default, so a missing key never disables a
// security control. Unknown keys are rejected to catch typos, and all values
// are range-checked on startup with actionable messages.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"flap/internal/game"
)

// Duration is a time.Duration that reads from strings such as "30s".
type Duration struct{ time.Duration }

// D builds a Duration.
func D(d time.Duration) Duration { return Duration{d} }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q (use values like \"30s\", \"5m\", \"2h\")", string(b))
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Authentication modes.
const (
	// AuthOpen accepts any public key as a persistent identity and lets
	// players without a key in as guests.
	AuthOpen = "open"
	// AuthPublicKey accepts any public key; keyless clients are refused.
	AuthPublicKey = "publickey"
	// AuthAllowlist only accepts public keys listed in the authorized keys
	// file.
	AuthAllowlist = "allowlist"
)

// Config is the complete service configuration.
type Config struct {
	Server  Server  `toml:"server"`
	Auth    Auth    `toml:"auth"`
	Limits  Limits  `toml:"limits"`
	UI      UI      `toml:"ui"`
	Game    Game    `toml:"game"`
	Storage Storage `toml:"storage"`
	Logging Logging `toml:"logging"`
}

// Server holds network and identity settings.
type Server struct {
	Listen             string   `toml:"listen"`
	HostKeyFiles       []string `toml:"host_key_files"`
	GenerateHostKey    bool     `toml:"generate_host_key"`
	IdentitySecretFile string   `toml:"identity_secret_file"`
	Name               string   `toml:"name"`
	Maintenance        bool     `toml:"maintenance"`
	MaintenanceMessage string   `toml:"maintenance_message"`
	ShutdownGrace      Duration `toml:"shutdown_grace"`
}

// Auth selects the authentication policy.
type Auth struct {
	Mode               string `toml:"mode"`
	AuthorizedKeysFile string `toml:"authorized_keys_file"`
	GuestLeaderboard   bool   `toml:"guest_leaderboard"`
}

// Limits bounds the resources any client can consume.
type Limits struct {
	MaxConnections        int      `toml:"max_connections"`
	MaxUnauthenticated    int      `toml:"max_unauthenticated"`
	MaxSessions           int      `toml:"max_sessions"`
	MaxConnectionsPerIP   int      `toml:"max_connections_per_ip"`
	MaxSessionsPerPlayer  int      `toml:"max_sessions_per_player"`
	ConnectionRatePerIP   float64  `toml:"connection_rate_per_ip"`
	ConnectionBurstPerIP  int      `toml:"connection_burst_per_ip"`
	ConnectionRateGlobal  float64  `toml:"connection_rate_global"`
	ConnectionBurstGlobal int      `toml:"connection_burst_global"`
	AuthFailuresBeforeBan int      `toml:"auth_failures_before_ban"`
	AuthFailureWindow     Duration `toml:"auth_failure_window"`
	BanDuration           Duration `toml:"ban_duration"`
	TrackedAddresses      int      `toml:"tracked_addresses"`
	HandshakeTimeout      Duration `toml:"handshake_timeout"`
	SessionStartTimeout   Duration `toml:"session_start_timeout"`
	IdleTimeout           Duration `toml:"idle_timeout"`
	MaxSessionDuration    Duration `toml:"max_session_duration"`
	WriteTimeout          Duration `toml:"write_timeout"`
	KeepaliveInterval     Duration `toml:"keepalive_interval"`
	KeepaliveMaxMissed    int      `toml:"keepalive_max_missed"`
	InputBytesPerSecond   int      `toml:"input_bytes_per_second"`
	InputBurstBytes       int      `toml:"input_burst_bytes"`
	MaxTerminalWidth      int      `toml:"max_terminal_width"`
	MaxTerminalHeight     int      `toml:"max_terminal_height"`
}

// UI configures rendering.
type UI struct {
	RenderFPS     int    `toml:"render_fps"`
	MenuFPS       int    `toml:"menu_fps"`
	MinWidth      int    `toml:"min_width"`
	MinHeight     int    `toml:"min_height"`
	DefaultColor  string `toml:"default_color"`
	DefaultGlyphs string `toml:"default_glyphs"`
}

// Game configures physics and difficulty.
type Game struct {
	TickRate          int          `toml:"tick_rate"`
	Gravity           float64      `toml:"gravity"`
	FlapVelocity      float64      `toml:"flap_velocity"`
	MaxFallSpeed      float64      `toml:"max_fall_speed"`
	PipeWidth         float64      `toml:"pipe_width"`
	GapMargin         float64      `toml:"gap_margin"`
	FirstPipeDistance float64      `toml:"first_pipe_distance"`
	Stages            []game.Stage `toml:"stages"`
}

// Storage configures persistence and retention.
type Storage struct {
	Path                string   `toml:"path"`
	OperationTimeout    Duration `toml:"operation_timeout"`
	LeaderboardSize     int      `toml:"leaderboard_size"`
	RunRetentionDays    int      `toml:"run_retention_days"`
	MaxRunsPerPlayer    int      `toml:"max_runs_per_player"`
	GuestRetentionDays  int      `toml:"guest_retention_days"`
	IntegrityCheck      string   `toml:"integrity_check"`
	MaintenanceInterval Duration `toml:"maintenance_interval"`
}

// Logging configures structured logs.
type Logging struct {
	Level         string   `toml:"level"`
	Format        string   `toml:"format"`
	IPAddresses   string   `toml:"ip_addresses"`
	StatsInterval Duration `toml:"stats_interval"`
}

// Default returns the default configuration. It is suitable for a small VPS.
func Default() Config {
	p := game.DefaultParams()
	return Config{
		Server: Server{
			Listen:             "0.0.0.0:2222",
			HostKeyFiles:       []string{"/var/lib/flapd/ssh_host_ed25519_key"},
			GenerateHostKey:    true,
			IdentitySecretFile: "/var/lib/flapd/identity_secret",
			Name:               "",
			MaintenanceMessage: "Flap is down for maintenance. Please try again later.",
			ShutdownGrace:      D(10 * time.Second),
		},
		Auth: Auth{
			Mode:             AuthOpen,
			GuestLeaderboard: true,
		},
		Limits: Limits{
			MaxConnections:        64,
			MaxUnauthenticated:    16,
			MaxSessions:           20,
			MaxConnectionsPerIP:   3,
			MaxSessionsPerPlayer:  2,
			ConnectionRatePerIP:   12,
			ConnectionBurstPerIP:  6,
			ConnectionRateGlobal:  20,
			ConnectionBurstGlobal: 40,
			AuthFailuresBeforeBan: 10,
			AuthFailureWindow:     D(10 * time.Minute),
			BanDuration:           D(15 * time.Minute),
			TrackedAddresses:      10000,
			HandshakeTimeout:      D(10 * time.Second),
			SessionStartTimeout:   D(10 * time.Second),
			IdleTimeout:           D(10 * time.Minute),
			MaxSessionDuration:    D(2 * time.Hour),
			WriteTimeout:          D(20 * time.Second),
			KeepaliveInterval:     D(30 * time.Second),
			KeepaliveMaxMissed:    3,
			InputBytesPerSecond:   2048,
			InputBurstBytes:       8192,
			MaxTerminalWidth:      400,
			MaxTerminalHeight:     200,
		},
		UI: UI{
			RenderFPS:     30,
			MenuFPS:       12,
			MinWidth:      50,
			MinHeight:     18,
			DefaultColor:  "auto",
			DefaultGlyphs: "auto",
		},
		Game: Game{
			TickRate:          p.TickRate,
			Gravity:           p.Gravity,
			FlapVelocity:      p.FlapVelocity,
			MaxFallSpeed:      p.MaxFallSpeed,
			PipeWidth:         p.PipeWidth,
			GapMargin:         p.GapMargin,
			FirstPipeDistance: p.FirstPipe,
			Stages:            p.Stages,
		},
		Storage: Storage{
			Path:                "/var/lib/flapd/flap.db",
			OperationTimeout:    D(3 * time.Second),
			LeaderboardSize:     100,
			RunRetentionDays:    365,
			MaxRunsPerPlayer:    500,
			GuestRetentionDays:  30,
			IntegrityCheck:      "quick",
			MaintenanceInterval: D(time.Hour),
		},
		Logging: Logging{
			Level:         "info",
			Format:        "json",
			IPAddresses:   "truncated",
			StatsInterval: D(5 * time.Minute),
		},
	}
}

// Load reads a TOML file on top of the defaults and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := Decode(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: invalid configuration:\n%w", path, err)
	}
	return cfg, nil
}

// Decode parses TOML into cfg, rejecting unknown keys. A [[game.stages]]
// table in the input replaces the default stages entirely.
func Decode(data []byte, cfg *Config) error {
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if bytes.Contains(data, []byte("[[game.stages]]")) {
		cfg.Game.Stages = nil
	}
	if err := dec.Decode(cfg); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return fmt.Errorf("unknown configuration keys (check for typos):\n%s", strict.String())
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			return fmt.Errorf("line %d, column %d: %s", row, col, de.Error())
		}
		return err
	}
	return nil
}

// GameParams converts the game section into simulation parameters.
func (c Config) GameParams() game.Params {
	p := game.DefaultParams()
	p.TickRate = c.Game.TickRate
	p.Gravity = c.Game.Gravity
	p.FlapVelocity = c.Game.FlapVelocity
	p.MaxFallSpeed = c.Game.MaxFallSpeed
	p.PipeWidth = c.Game.PipeWidth
	p.GapMargin = c.Game.GapMargin
	p.FirstPipe = c.Game.FirstPipeDistance
	p.Stages = append([]game.Stage(nil), c.Game.Stages...)
	return p
}

type validator struct{ errs []string }

func (v *validator) add(format string, args ...any) {
	v.errs = append(v.errs, "  - "+fmt.Sprintf(format, args...))
}

func (v *validator) intRange(key string, val, lo, hi int) {
	if val < lo || val > hi {
		v.add("%s = %d: must be between %d and %d", key, val, lo, hi)
	}
}

func (v *validator) floatRange(key string, val, lo, hi float64) {
	if math.IsNaN(val) || val < lo || val > hi {
		v.add("%s = %g: must be between %g and %g", key, val, lo, hi)
	}
}

func (v *validator) durRange(key string, d Duration, lo, hi time.Duration) {
	if d.Duration < lo || d.Duration > hi {
		v.add("%s = %q: must be between %s and %s", key, d.String(), lo, hi)
	}
}

func (v *validator) oneOf(key, val string, allowed ...string) {
	for _, a := range allowed {
		if val == a {
			return
		}
	}
	v.add("%s = %q: must be one of %s", key, val, strings.Join(allowed, ", "))
}

// Validate checks every setting and returns all problems at once.
func (c Config) Validate() error {
	v := &validator{}

	// Server.
	if host, port, err := net.SplitHostPort(c.Server.Listen); err != nil {
		v.add("server.listen = %q: must be host:port, for example \"0.0.0.0:2222\"", c.Server.Listen)
	} else {
		if host != "" && net.ParseIP(host) == nil && host != "localhost" {
			v.add("server.listen = %q: host must be an IP address (or empty for all interfaces)", c.Server.Listen)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			v.add("server.listen = %q: port must be 1-65535", c.Server.Listen)
		}
	}
	if len(c.Server.HostKeyFiles) == 0 {
		v.add("server.host_key_files: at least one host key file is required")
	}
	for i, f := range c.Server.HostKeyFiles {
		if strings.TrimSpace(f) == "" {
			v.add("server.host_key_files[%d]: path must not be empty", i)
		}
	}
	if strings.TrimSpace(c.Server.IdentitySecretFile) == "" {
		v.add("server.identity_secret_file: path must not be empty")
	}
	if len(c.Server.Name) > 40 || !printableASCII(c.Server.Name) {
		v.add("server.name: must be at most 40 printable ASCII characters")
	}
	if len(c.Server.MaintenanceMessage) > 200 || !printableASCII(c.Server.MaintenanceMessage) {
		v.add("server.maintenance_message: must be at most 200 printable ASCII characters")
	}
	v.durRange("server.shutdown_grace", c.Server.ShutdownGrace, 0, 5*time.Minute)

	// Auth.
	v.oneOf("auth.mode", c.Auth.Mode, AuthOpen, AuthPublicKey, AuthAllowlist)
	if c.Auth.Mode == AuthAllowlist && strings.TrimSpace(c.Auth.AuthorizedKeysFile) == "" {
		v.add("auth.authorized_keys_file: required when auth.mode = %q", AuthAllowlist)
	}

	// Limits.
	l := c.Limits
	v.intRange("limits.max_connections", l.MaxConnections, 1, 100000)
	v.intRange("limits.max_unauthenticated", l.MaxUnauthenticated, 1, 10000)
	v.intRange("limits.max_sessions", l.MaxSessions, 1, 10000)
	v.intRange("limits.max_connections_per_ip", l.MaxConnectionsPerIP, 1, 1000)
	v.intRange("limits.max_sessions_per_player", l.MaxSessionsPerPlayer, 1, 100)
	if l.MaxSessions > l.MaxConnections {
		v.add("limits.max_sessions (%d) must not exceed limits.max_connections (%d)", l.MaxSessions, l.MaxConnections)
	}
	if l.MaxUnauthenticated > l.MaxConnections {
		v.add("limits.max_unauthenticated (%d) must not exceed limits.max_connections (%d)", l.MaxUnauthenticated, l.MaxConnections)
	}
	v.floatRange("limits.connection_rate_per_ip (per minute)", l.ConnectionRatePerIP, 0.1, 6000)
	v.intRange("limits.connection_burst_per_ip", l.ConnectionBurstPerIP, 1, 1000)
	v.floatRange("limits.connection_rate_global (per second)", l.ConnectionRateGlobal, 0.1, 10000)
	v.intRange("limits.connection_burst_global", l.ConnectionBurstGlobal, 1, 100000)
	v.intRange("limits.auth_failures_before_ban", l.AuthFailuresBeforeBan, 1, 1000)
	v.durRange("limits.auth_failure_window", l.AuthFailureWindow, time.Second, 24*time.Hour)
	v.durRange("limits.ban_duration", l.BanDuration, time.Second, 7*24*time.Hour)
	v.intRange("limits.tracked_addresses", l.TrackedAddresses, 100, 10000000)
	v.durRange("limits.handshake_timeout", l.HandshakeTimeout, time.Second, 2*time.Minute)
	v.durRange("limits.session_start_timeout", l.SessionStartTimeout, time.Second, 2*time.Minute)
	v.durRange("limits.idle_timeout", l.IdleTimeout, 5*time.Second, 24*time.Hour)
	if l.MaxSessionDuration.Duration != 0 {
		v.durRange("limits.max_session_duration (0 disables)", l.MaxSessionDuration, time.Minute, 7*24*time.Hour)
	}
	v.durRange("limits.write_timeout", l.WriteTimeout, time.Second, 5*time.Minute)
	v.durRange("limits.keepalive_interval", l.KeepaliveInterval, 5*time.Second, 10*time.Minute)
	v.intRange("limits.keepalive_max_missed", l.KeepaliveMaxMissed, 1, 100)
	v.intRange("limits.input_bytes_per_second", l.InputBytesPerSecond, 64, 1<<20)
	v.intRange("limits.input_burst_bytes", l.InputBurstBytes, 256, 1<<22)
	v.intRange("limits.max_terminal_width", l.MaxTerminalWidth, 80, 2000)
	v.intRange("limits.max_terminal_height", l.MaxTerminalHeight, 24, 1000)

	// UI.
	u := c.UI
	v.intRange("ui.render_fps", u.RenderFPS, 5, 60)
	v.intRange("ui.menu_fps", u.MenuFPS, 2, 60)
	if u.MenuFPS > u.RenderFPS {
		v.add("ui.menu_fps (%d) must not exceed ui.render_fps (%d)", u.MenuFPS, u.RenderFPS)
	}
	v.intRange("ui.min_width", u.MinWidth, 40, l.MaxTerminalWidth)
	v.intRange("ui.min_height", u.MinHeight, 14, l.MaxTerminalHeight)
	v.oneOf("ui.default_color", u.DefaultColor, "auto", "truecolor", "256", "16")
	v.oneOf("ui.default_glyphs", u.DefaultGlyphs, "auto", "unicode", "ascii")

	// Game.
	if err := c.GameParams().Validate(); err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			v.add("game: %s", line)
		}
	}

	// Storage.
	s := c.Storage
	if strings.TrimSpace(s.Path) == "" {
		v.add("storage.path: must not be empty")
	}
	v.durRange("storage.operation_timeout", s.OperationTimeout, 100*time.Millisecond, time.Minute)
	v.intRange("storage.leaderboard_size", s.LeaderboardSize, 1, 1000)
	v.intRange("storage.run_retention_days (0 keeps runs forever)", s.RunRetentionDays, 0, 36500)
	v.intRange("storage.max_runs_per_player", s.MaxRunsPerPlayer, 10, 1000000)
	v.intRange("storage.guest_retention_days", s.GuestRetentionDays, 1, 36500)
	v.oneOf("storage.integrity_check", s.IntegrityCheck, "quick", "full", "off")
	v.durRange("storage.maintenance_interval", s.MaintenanceInterval, time.Minute, 7*24*time.Hour)

	// Logging.
	v.oneOf("logging.level", c.Logging.Level, "debug", "info", "warn", "error")
	v.oneOf("logging.format", c.Logging.Format, "json", "text")
	v.oneOf("logging.ip_addresses", c.Logging.IPAddresses, "full", "truncated", "hashed", "none")
	if c.Logging.StatsInterval.Duration != 0 {
		v.durRange("logging.stats_interval (0 disables)", c.Logging.StatsInterval, 10*time.Second, 24*time.Hour)
	}

	if len(v.errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(v.errs, "\n"))
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
