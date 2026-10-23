// Command flapd serves the Flap terminal game over SSH.
//
// Usage:
//
//	flapd serve        -config /etc/flapd/flapd.toml
//	flapd check-config -config /etc/flapd/flapd.toml
//	flapd init         -config /etc/flapd/flapd.toml
//	flapd keygen       -out /var/lib/flapd/ssh_host_ed25519_key
//	flapd fingerprint  -config /etc/flapd/flapd.toml
//	flapd check-db     -config /etc/flapd/flapd.toml [-full]
//	flapd backup       -config /etc/flapd/flapd.toml -out /path/backup.db
//	flapd remove-player -config /etc/flapd/flapd.toml -name NAME
//	flapd healthcheck  -config /etc/flapd/flapd.toml
//	flapd play         [-db ./flap-local.db]
//	flapd version
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/crypto/ssh"

	"flap/internal/canvas"
	"flap/internal/config"
	"flap/internal/sdnotify"
	"flap/internal/security"
	"flap/internal/sshserver"
	"flap/internal/store"
	"flap/internal/tui"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func buildVersion() string {
	v := version
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				v += "+" + s.Value[:12]
			}
		}
		v += " (" + bi.GoVersion + ")"
	}
	return v
}

const usage = `flapd: the Flap SSH game server

Commands:
  serve         run the SSH game server
  check-config  validate a configuration file
  init          create missing host keys and the identity secret (idempotent)
  keygen        create an Ed25519 SSH host key
  fingerprint   print the host key fingerprints players should see
  check-db      run database integrity checks
  backup        write a consistent copy of the database (safe while running)
  remove-player delete a player and their runs from the leaderboard
  healthcheck   verify the running server completes an SSH handshake
  play          play locally in this terminal (development)
  version       print the version

Run "flapd COMMAND -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "check-config":
		err = cmdCheckConfig(args)
	case "keygen":
		err = cmdKeygen(args)
	case "init":
		err = cmdInit(args)
	case "fingerprint":
		err = cmdFingerprint(args)
	case "check-db":
		err = cmdCheckDB(args)
	case "backup":
		err = cmdBackup(args)
	case "remove-player":
		err = cmdRemovePlayer(args)
	case "healthcheck":
		err = cmdHealthcheck(args)
	case "play":
		err = cmdPlay(args)
	case "version", "-version", "--version":
		fmt.Println("flapd", buildVersion())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "flapd: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "flapd:", err)
		os.Exit(1)
	}
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "/etc/flapd/flapd.toml", "configuration file")
}

func loadConfig(path string) (config.Config, error) {
	return config.Load(path)
}

func newLogger(c config.Logging, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func cmdCheckConfig(args []string) error {
	fs := flag.NewFlagSet("check-config", flag.ExitOnError)
	path := configFlag(fs)
	listen := fs.Bool("listen", false, "print only the listen address (for scripts)")
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if *listen {
		fmt.Println(cfg.Server.Listen)
		return nil
	}
	fmt.Printf("%s: OK (listen %s, auth %s, max %d sessions)\n", *path, cfg.Server.Listen, cfg.Auth.Mode, cfg.Limits.MaxSessions)
	return nil
}

// cmdInit provisions the host keys and identity secret named in the
// configuration. It is idempotent: existing files are kept.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	keys, generated, err := sshserver.LoadHostKeys(cfg.Server.HostKeyFiles, true)
	if err != nil {
		return err
	}
	for _, g := range generated {
		fmt.Printf("created host key %s\n", g)
	}
	_, created, err := sshserver.LoadSecret(cfg.Server.IdentitySecretFile)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("created identity secret %s\n", cfg.Server.IdentitySecretFile)
	}
	for _, k := range keys {
		fmt.Printf("host key fingerprint: %s\n", ssh.FingerprintSHA256(k.PublicKey()))
	}
	return nil
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "path of the new private key (must not exist)")
	fs.Parse(args)
	if *out == "" {
		return errors.New("keygen: -out is required")
	}
	s, err := sshserver.GenerateHostKey(*out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s\n%s\n", *out, ssh.FingerprintSHA256(s.PublicKey()))
	return nil
}

func cmdFingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	keys, _, err := sshserver.LoadHostKeys(cfg.Server.HostKeyFiles, false)
	if err != nil {
		return err
	}
	for _, k := range keys {
		fmt.Printf("%s %s\n", ssh.FingerprintSHA256(k.PublicKey()), k.PublicKey().Type())
		fmt.Print(string(ssh.MarshalAuthorizedKey(k.PublicKey())))
	}
	return nil
}

func openStore(ctx context.Context, cfg config.Config, check string, log *slog.Logger) (*store.Store, error) {
	return store.Open(ctx, cfg.Storage.Path, store.Options{IntegrityCheck: check, GuestLeaderboard: cfg.Auth.GuestLeaderboard, Logger: log})
}

func cmdCheckDB(args []string) error {
	fs := flag.NewFlagSet("check-db", flag.ExitOnError)
	path := configFlag(fs)
	full := fs.Bool("full", false, "run the full (slower) integrity check")
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := openStore(ctx, cfg, "off", nil)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Check(ctx, *full); err != nil {
		return err
	}
	v, _ := st.SchemaVersion(ctx)
	players, runs, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%s: OK (schema v%d, %d players, %d runs)\n", cfg.Storage.Path, v, players, runs)
	return nil
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	path := configFlag(fs)
	out := fs.String("out", "", "backup file to create (must not exist)")
	fs.Parse(args)
	if *out == "" {
		return errors.New("backup: -out is required")
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := openStore(ctx, cfg, "quick", nil)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Backup(ctx, *out); err != nil {
		return err
	}
	// Verify the copy before declaring success.
	bk, err := store.Open(ctx, *out, store.Options{IntegrityCheck: "full"})
	if err != nil {
		return fmt.Errorf("backup verification failed: %w", err)
	}
	players, runs, _ := bk.Stats(ctx)
	bk.Close()
	fmt.Printf("wrote %s (%d players, %d runs)\n", *out, players, runs)
	fmt.Println("Also keep a copy of the host key and identity secret files; see docs/OPERATIONS.md.")
	return nil
}

func cmdRemovePlayer(args []string) error {
	fs := flag.NewFlagSet("remove-player", flag.ExitOnError)
	path := configFlag(fs)
	name := fs.String("name", "", "exact display name of the player to remove")
	fs.Parse(args)
	if *name == "" {
		return errors.New("remove-player: -name is required")
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := openStore(ctx, cfg, "off", nil)
	if err != nil {
		return err
	}
	defer st.Close()
	p, ok, err := st.RemovePlayer(ctx, *name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no player named %q", *name)
	}
	fmt.Printf("removed %s (%s, best %d, %d runs)\n", p.Name, p.Kind, p.Best, p.Runs)
	return nil
}

var errHostKeyOK = errors.New("host key verified")

func cmdHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	path := configFlag(fs)
	addr := fs.String("addr", "", "address to check (default: derived from server.listen)")
	timeout := fs.Duration("timeout", 5*time.Second, "timeout")
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	keys, _, err := sshserver.LoadHostKeys(cfg.Server.HostKeyFiles, false)
	if err != nil {
		return err
	}
	target := *addr
	if target == "" {
		host, port, _ := net.SplitHostPort(cfg.Server.Listen)
		switch host {
		case "", "0.0.0.0":
			host = "127.0.0.1"
		case "::":
			host = "::1"
		}
		target = net.JoinHostPort(host, port)
	}
	conn, err := net.DialTimeout("tcp", target, *timeout)
	if err != nil {
		return fmt.Errorf("unhealthy: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(*timeout))
	// Stop right after the host key is verified so the probe never counts
	// as a failed login.
	ccfg := &ssh.ClientConfig{
		User: "healthcheck",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			for _, k := range keys {
				if string(k.PublicKey().Marshal()) == string(key.Marshal()) {
					return errHostKeyOK
				}
			}
			return fmt.Errorf("server presented an unexpected host key %s", ssh.FingerprintSHA256(key))
		},
		Timeout: *timeout,
	}
	_, _, _, err = ssh.NewClientConn(conn, target, ccfg)
	if errors.Is(err, errHostKeyOK) {
		fmt.Printf("healthy: %s answered with the expected host key\n", target)
		return nil
	}
	return fmt.Errorf("unhealthy: %w", err)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := configFlag(fs)
	fs.Parse(args)
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Logging, os.Stderr)
	slog.SetDefault(log)
	log.Info("starting flapd", "version", buildVersion(), "config", *path)

	keys, generated, err := sshserver.LoadHostKeys(cfg.Server.HostKeyFiles, cfg.Server.GenerateHostKey)
	if err != nil {
		return err
	}
	for _, g := range generated {
		log.Warn("generated a new SSH host key; players will be asked to trust it", "path", g)
	}
	secret, created, err := sshserver.LoadSecret(cfg.Server.IdentitySecretFile)
	if err != nil {
		return err
	}
	if created {
		log.Warn("created a new identity secret; back it up together with the database", "path", cfg.Server.IdentitySecretFile)
	}
	var allow *sshserver.Allowlist
	if cfg.Auth.Mode == config.AuthAllowlist {
		if allow, err = sshserver.LoadAllowlist(cfg.Auth.AuthorizedKeysFile); err != nil {
			return err
		}
		log.Info("loaded allowlist", "keys", allow.Len())
	}

	ctx := context.Background()
	st, err := openStore(ctx, cfg, cfg.Storage.IntegrityCheck, log)
	if err != nil {
		return err
	}
	defer st.Close()

	srv, err := sshserver.New(sshserver.Options{Config: cfg, Store: st, HostKeys: keys, Secret: secret, Allowlist: allow, Logger: log})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("cannot listen on %s: the port is already in use by another process.\n"+
				"Check with: ss -ltnp 'sport = :%s'  and pick a free port in server.listen", cfg.Server.Listen, portOf(cfg.Server.Listen))
		}
		if errors.Is(err, syscall.EACCES) {
			return fmt.Errorf("cannot listen on %s: permission denied (ports below 1024 need extra privileges; use a port such as 2222)", cfg.Server.Listen)
		}
		return fmt.Errorf("listen: %w", err)
	}
	fps := make([]string, 0, len(keys))
	for _, k := range keys {
		fps = append(fps, ssh.FingerprintSHA256(k.PublicKey()))
	}
	log.Info("listening", "addr", ln.Addr().String(), "auth", cfg.Auth.Mode, "host_keys", strings.Join(fps, ","),
		"max_sessions", cfg.Limits.MaxSessions, "maintenance", cfg.Server.Maintenance)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	maintCtx, stopMaint := context.WithCancel(ctx)
	maintDone := make(chan struct{})
	go func() {
		defer close(maintDone)
		maintenance(maintCtx, st, cfg, log)
	}()

	if ok, err := sdnotify.Notify("READY=1\nSTATUS=accepting connections"); err != nil {
		log.Warn("sd_notify failed", "err", err)
	} else if ok {
		log.Debug("notified systemd")
	}

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	for {
		select {
		case err := <-serveErr:
			stopMaint()
			<-maintDone
			return fmt.Errorf("server stopped: %w", err)
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				if cfg.Auth.Mode == config.AuthAllowlist {
					if a, err := sshserver.LoadAllowlist(cfg.Auth.AuthorizedKeysFile); err != nil {
						log.Error("allowlist reload failed; keeping the old one", "err", err)
					} else {
						srv.SetAllowlist(a)
						log.Info("reloaded allowlist", "keys", a.Len())
					}
				} else {
					log.Info("SIGHUP ignored: only the allowlist can be reloaded")
				}
				continue
			}
			log.Info("signal received; shutting down", "signal", sig.String())
			sdnotify.Notify("STOPPING=1")
			sctx, cancel := context.WithTimeout(ctx, cfg.Server.ShutdownGrace.Duration+15*time.Second)
			err := srv.Shutdown(sctx)
			cancel()
			stopMaint()
			<-maintDone
			if err != nil {
				log.Error("shutdown incomplete", "err", err)
			}
			<-serveErr
			log.Info("stopped")
			return nil
		}
	}
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

// maintenance applies the retention policy periodically.
func maintenance(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) {
	ret := store.Retention{
		RunMaxAge:        time.Duration(cfg.Storage.RunRetentionDays) * 24 * time.Hour,
		MaxRunsPerPlayer: cfg.Storage.MaxRunsPerPlayer,
		GuestMaxIdle:     time.Duration(cfg.Storage.GuestRetentionDays) * 24 * time.Hour,
		KeepTop:          cfg.Storage.LeaderboardSize,
	}
	run := func() {
		c, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		res, err := st.Prune(c, ret)
		if err != nil {
			log.Error("retention pruning failed", "err", err)
			return
		}
		if res.OldRuns+res.ExcessRuns+res.Guests > 0 {
			log.Info("pruned old data", "old_runs", res.OldRuns, "excess_runs", res.ExcessRuns, "guests", res.Guests)
		}
	}
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
		run()
	}
	t := time.NewTicker(cfg.Storage.MaintenanceInterval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// cmdPlay runs the game in the local terminal, for development and for
// trying the game without an SSH server.
func cmdPlay(args []string) error {
	fs := flag.NewFlagSet("play", flag.ExitOnError)
	dbPath := fs.String("db", "flap-local.db", "local score database")
	colorMode := fs.String("color", "truecolor", "truecolor, 256 or 16")
	ascii := fs.Bool("ascii", false, "use only ASCII characters")
	fs.Parse(args)
	cfg := config.Default()
	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath, store.Options{IntegrityCheck: "quick", GuestLeaderboard: true})
	if err != nil {
		return err
	}
	defer st.Close()
	name := "LocalPilot"
	if u, err := user.Current(); err == nil {
		if n := security.SanitizeName(u.Username); security.ValidateName(n) == nil {
			name = n
		}
	}
	// The local player has a fixed identity per database file.
	abs, _ := filepath.Abs(*dbPath)
	sum := sha256.Sum256([]byte("flap-local:" + abs))
	p, err := st.KeyPlayer(ctx, hex.EncodeToString(sum[:]), func(a int) string {
		if a == 0 {
			return name
		}
		return fmt.Sprintf("%s%d", name[:min(len(name), 13)], a)
	})
	if err != nil {
		return err
	}
	cm, ok := canvas.ParseColorMode(*colorMode)
	if !ok {
		return fmt.Errorf("unknown color mode %q", *colorMode)
	}
	glyphs := tui.Unicode
	if *ascii {
		glyphs = tui.ASCII
	}
	sound, _ := tui.ParseSoundMode(cfg.UI.DefaultSound)
	if m, ok := tui.ParseSoundMode(p.SoundMode); ok {
		sound = m
	}
	app := tui.New(st, tui.Session{Player: p, KeyFingerprint: "local", Color: cm, Glyphs: glyphs, Sound: sound, Width: 80, Height: 24},
		tui.Options{
			Params: cfg.GameParams(), RenderFPS: cfg.UI.RenderFPS, MenuFPS: cfg.UI.MenuFPS,
			MinWidth: cfg.UI.MinWidth, MinHeight: cfg.UI.MinHeight, LeaderboardSize: cfg.Storage.LeaderboardSize,
			StoreTimeout: cfg.Storage.OperationTimeout.Duration, ServerName: "local",
		})
	prog := tea.NewProgram(app, tea.WithFPS(cfg.UI.RenderFPS))
	if _, err := prog.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	if _, err := app.SaveUnsaved(ctx, store.ReasonQuit); err != nil {
		return err
	}
	fmt.Printf("Thanks for playing Flap, %s! Best score: %d\n", app.PlayerName(), app.Best())
	return nil
}
