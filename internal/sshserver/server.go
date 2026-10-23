// Package sshserver runs the public SSH endpoint. It speaks just enough SSH
// to host an interactive game: one session channel per connection, a PTY
// and a "shell" request that starts the game. Shells, commands, subsystems,
// port forwarding, agent forwarding and X11 are all refused.
package sshserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"flap/internal/config"
	"flap/internal/security"
	"flap/internal/store"
)

// Permission extension keys carried from authentication to the session.
const (
	extKind        = "flap-kind"
	extIdentity    = "flap-identity"
	extFingerprint = "flap-fingerprint"
	kindKey        = "key"
	kindGuest      = "guest"
)

// Store is the persistence used by sessions.
type Store interface {
	KeyPlayer(ctx context.Context, identity string, nameFor func(int) string) (store.Player, error)
	CreateGuest(ctx context.Context, nameFor func(int) string) (store.Player, error)
	RecordRun(ctx context.Context, playerID int64, r store.Run) (store.RunResult, error)
	Leaderboard(ctx context.Context, offset, limit int) (store.Page, error)
	Rank(ctx context.Context, playerID int64) (int, int, error)
	RecentRuns(ctx context.Context, playerID int64, limit int) ([]store.RunSummary, error)
	Rename(ctx context.Context, playerID int64, name string) error
	SetPrefs(ctx context.Context, playerID int64, colorMode, glyphMode string) error
}

// Options configures a Server.
type Options struct {
	Config   config.Config
	Store    Store
	HostKeys []ssh.Signer
	// Secret derives stable player identities from public keys.
	Secret    []byte
	Allowlist *Allowlist
	Logger    *slog.Logger
}

// Server is the SSH game server.
type Server struct {
	cfg    config.Config
	store  Store
	log    *slog.Logger
	secret []byte
	ssh    *ssh.ServerConfig

	allow     atomic.Pointer[Allowlist]
	admission *security.Admission
	conns     *security.Semaphore
	unauth    *security.Semaphore
	sessions  *registry
	redact    security.IPRedactor

	mu       sync.Mutex
	ln       net.Listener
	active   map[net.Conn]struct{}
	closing  atomic.Bool
	wg       sync.WaitGroup
	hardStop context.Context
	stopAll  context.CancelFunc

	stats stats
}

type stats struct {
	accepted, rejected, authFailed, sessions atomic.Int64
	bytesIn, bytesOut                        atomic.Int64
	rejectReasons                            sync.Map // string -> *atomic.Int64
}

func (s *stats) reject(reason string) {
	s.rejected.Add(1)
	v, _ := s.rejectReasons.LoadOrStore(reason, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// New builds a server.
func New(opt Options) (*Server, error) {
	cfg := opt.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(opt.HostKeys) == 0 {
		return nil, errors.New("at least one host key is required")
	}
	if len(opt.Secret) < 32 {
		return nil, errors.New("identity secret must be at least 32 bytes")
	}
	if cfg.Auth.Mode == config.AuthAllowlist && opt.Allowlist == nil {
		return nil, errors.New("allowlist mode requires an allowlist")
	}
	log := opt.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	l := cfg.Limits
	redactKey := make([]byte, 32)
	if _, err := rand.Read(redactKey); err != nil {
		return nil, err
	}
	hardStop, stopAll := context.WithCancel(context.Background())
	s := &Server{
		cfg:    cfg,
		store:  opt.Store,
		log:    log,
		secret: opt.Secret,
		admission: security.NewAdmission(security.AdmissionConfig{
			MaxConnsPerIP:     l.MaxConnectionsPerIP,
			RatePerIP:         l.ConnectionRatePerIP / 60,
			BurstPerIP:        l.ConnectionBurstPerIP,
			GlobalRate:        l.ConnectionRateGlobal,
			GlobalBurst:       l.ConnectionBurstGlobal,
			FailuresBeforeBan: l.AuthFailuresBeforeBan,
			FailureWindow:     l.AuthFailureWindow.Duration,
			BanDuration:       l.BanDuration.Duration,
			MaxTracked:        l.TrackedAddresses,
		}, nil),
		conns:    security.NewSemaphore(l.MaxConnections),
		unauth:   security.NewSemaphore(l.MaxUnauthenticated),
		sessions: newRegistry(l.MaxSessions, l.MaxSessionsPerPlayer),
		redact:   security.NewIPRedactor(cfg.Logging.IPAddresses, redactKey),
		active:   map[net.Conn]struct{}{},
		hardStop: hardStop,
		stopAll:  stopAll,
	}
	s.allow.Store(opt.Allowlist)
	s.ssh = s.serverConfig(opt.HostKeys)
	return s, nil
}

// SetAllowlist replaces the allowlist, for example after SIGHUP.
func (s *Server) SetAllowlist(a *Allowlist) { s.allow.Store(a) }

func (s *Server) port() string {
	_, p, err := net.SplitHostPort(s.cfg.Server.Listen)
	if err != nil {
		return "2222"
	}
	return p
}

func (s *Server) hostHint() string {
	if s.cfg.Server.Name != "" {
		return s.cfg.Server.Name
	}
	return "HOST"
}

// ActiveSessions returns the number of running games.
func (s *Server) ActiveSessions() int { return s.sessions.count() }

func (s *Server) serverConfig(keys []ssh.Signer) *ssh.ServerConfig {
	sc := &ssh.ServerConfig{
		// No version number: there is no need to advertise the build.
		ServerVersion: "SSH-2.0-flapd",
		MaxAuthTries:  6,
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if err := acceptableKey(key); err != nil {
				return nil, err
			}
			if s.cfg.Auth.Mode == config.AuthAllowlist && !s.allow.Load().Contains(key) {
				return nil, errors.New("key not authorised")
			}
			return &ssh.Permissions{}, nil
		},
		// Identity is only derived once the client has proven it holds the
		// private key, never from a key that was merely offered.
		VerifiedPublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey, _ *ssh.Permissions, _ string) (*ssh.Permissions, error) {
			return &ssh.Permissions{Extensions: map[string]string{
				extKind:        kindKey,
				extIdentity:    s.identity(key),
				extFingerprint: ssh.FingerprintSHA256(key),
			}}, nil
		},
		AuthLogCallback: func(meta ssh.ConnMetadata, method string, err error) {
			if err == nil || method == "none" {
				return
			}
			s.stats.authFailed.Add(1)
			if s.admission.RecordFailure(security.AddrKey(meta.RemoteAddr())) {
				s.log.Warn("address banned after repeated authentication failures",
					"remote", s.redact.Redact(meta.RemoteAddr()), "duration", s.cfg.Limits.BanDuration.String())
			}
		},
	}
	if s.cfg.Auth.Mode == config.AuthOpen {
		sc.KeyboardInteractiveCallback = func(meta ssh.ConnMetadata, client ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			// No questions: this is only an informational round so keyless
			// clients learn they are playing as a guest.
			if _, err := client("", "No SSH key accepted: you will play as a guest.", nil, nil); err != nil {
				return nil, err
			}
			return &ssh.Permissions{Extensions: map[string]string{extKind: kindGuest}}, nil
		}
	}
	for _, k := range keys {
		sc.AddHostKey(k)
	}
	return sc
}

// identity derives the stable player identity for a public key. The keyed
// hash means a leaked database cannot be matched against published keys.
func (s *Server) identity(key ssh.PublicKey) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte("flap-player-v1\x00"))
	m.Write(key.Marshal())
	return hex.EncodeToString(m.Sum(nil))
}

// Serve accepts connections on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	if iv := s.cfg.Logging.StatsInterval.Duration; iv > 0 {
		s.wg.Go(func() { s.statsLoop(iv) })
	}
	s.wg.Go(s.sweepLoop)
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.closing.Load() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, os.ErrDeadlineExceeded) || isTemporary(err) {
				backoff = min(max(backoff*2, 5*time.Millisecond), time.Second)
				s.log.Warn("accept failed; backing off", "err", err, "backoff", backoff.String())
				time.Sleep(backoff)
				continue
			}
			return err
		}
		backoff = 0
		s.admit(conn)
	}
}

func isTemporary(err error) bool {
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

// admit applies admission control before any SSH processing happens, so a
// rejected connection costs a socket and nothing else.
func (s *Server) admit(conn net.Conn) {
	if !s.conns.TryAcquire() {
		s.stats.reject("max_connections")
		conn.Close()
		return
	}
	key := security.AddrKey(conn.RemoteAddr())
	release, why := s.admission.Admit(key)
	if why != security.Admitted {
		s.conns.Release()
		s.stats.reject(why.String())
		s.log.Debug("connection rejected", "reason", why.String(), "remote", s.redact.Redact(conn.RemoteAddr()))
		conn.Close()
		return
	}
	if !s.unauth.TryAcquire() {
		release()
		s.conns.Release()
		s.stats.reject("max_unauthenticated")
		conn.Close()
		return
	}
	s.mu.Lock()
	if s.closing.Load() {
		s.mu.Unlock()
		s.unauth.Release()
		release()
		s.conns.Release()
		conn.Close()
		return
	}
	s.active[conn] = struct{}{}
	s.mu.Unlock()
	s.stats.accepted.Add(1)
	s.wg.Go(func() {
		defer func() {
			s.mu.Lock()
			delete(s.active, conn)
			s.mu.Unlock()
			conn.Close()
			release()
			s.conns.Release()
		}()
		s.handleConn(conn)
	})
}

func (s *Server) handleConn(conn net.Conn) {
	start := time.Now()
	remote := s.redact.Redact(conn.RemoteAddr())
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 8192)
			buf = buf[:runtime.Stack(buf, false)]
			s.log.Error("panic in connection handler", "panic", fmt.Sprint(r), "stack", string(buf))
		}
	}()

	// The handshake slot is released exactly once, even if the handshake
	// panics.
	releaseUnauth := sync.OnceFunc(s.unauth.Release)
	defer releaseUnauth()
	// The deadline bounds key exchange and authentication together.
	_ = conn.SetDeadline(start.Add(s.cfg.Limits.HandshakeTimeout.Duration))
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.ssh)
	releaseUnauth()
	if err != nil {
		s.log.Debug("handshake failed", "remote", remote, "err", security.LogString(err.Error(), 200))
		return
	}
	_ = conn.SetDeadline(time.Time{})
	defer sconn.Close()

	perms := sconn.Permissions
	if perms == nil || perms.Extensions[extKind] == "" {
		s.log.Error("authenticated connection without identity; closing", "remote", remote)
		return
	}
	id := identity{
		kind:        perms.Extensions[extKind],
		key:         perms.Extensions[extIdentity],
		fingerprint: perms.Extensions[extFingerprint],
	}
	log := s.log.With("remote", remote, "player", id.short())
	log.Info("connection authenticated",
		"auth", id.kind, "user", security.LogString(sconn.User(), 32),
		"client", security.LogString(string(sconn.ClientVersion()), 64))

	// Global requests (port forwarding, keepalives from the client, ...)
	// are all refused.
	s.wg.Go(func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	})

	// Keepalives detect dead peers; a connection that stops answering is
	// closed.
	var lastAlive atomic.Int64
	lastAlive.Store(time.Now().UnixNano())
	connDone := make(chan struct{})
	defer close(connDone)
	s.wg.Go(func() { s.keepalive(sconn, &lastAlive, connDone, log) })

	// The client must open its session channel promptly.
	var started atomic.Bool
	startTimer := time.AfterFunc(s.cfg.Limits.SessionStartTimeout.Duration, func() {
		if !started.Load() {
			log.Info("no session started in time; closing")
			sconn.Close()
		}
	})
	defer startTimer.Stop()

	var sessionWG sync.WaitGroup
	sessionOpened := false
	for nc := range chans {
		switch {
		case nc.ChannelType() != "session":
			_ = nc.Reject(ssh.Prohibited, "only interactive sessions are supported")
			log.Info("refused channel", "type", security.LogString(nc.ChannelType(), 40))
		case sessionOpened:
			_ = nc.Reject(ssh.ResourceShortage, "only one session per connection")
		default:
			sessionOpened = true
			ch, creqs, err := nc.Accept()
			if err != nil {
				return
			}
			sessionWG.Go(func() {
				s.handleSession(sconn, ch, creqs, id, &started, &lastAlive, log)
				// One game per connection: end the connection with it.
				sconn.Close()
			})
		}
	}
	sessionWG.Wait()
	log.Info("connection closed", "duration", time.Since(start).Round(time.Millisecond).String())
}

func (s *Server) keepalive(sconn *ssh.ServerConn, lastAlive *atomic.Int64, done <-chan struct{}, log *slog.Logger) {
	iv := s.cfg.Limits.KeepaliveInterval.Duration
	maxSilence := iv * time.Duration(s.cfg.Limits.KeepaliveMaxMissed+1)
	t := time.NewTicker(iv)
	defer t.Stop()
	var inFlight atomic.Bool
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if time.Since(time.Unix(0, lastAlive.Load())) > maxSilence {
			log.Info("peer stopped answering keepalives; closing")
			sconn.Close()
			return
		}
		if inFlight.Load() {
			continue
		}
		inFlight.Store(true)
		// SendRequest blocks until the reply arrives or the connection
		// closes; any reply, even a failure, proves the peer is alive.
		s.wg.Go(func() {
			defer inFlight.Store(false)
			if _, _, err := sconn.SendRequest("keepalive@openssh.com", true, nil); err == nil {
				lastAlive.Store(time.Now().UnixNano())
			}
		})
	}
}

// Shutdown stops accepting connections, asks every player to wrap up, waits
// up to the grace period and then terminates whatever is left.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closing.Store(true)
	s.mu.Lock()
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Unlock()

	n := s.sessions.count()
	s.log.Info("shutting down", "sessions", n)
	s.sessions.notifyAll(noticeShutdown)
	grace := time.NewTimer(s.cfg.Server.ShutdownGrace.Duration)
	defer grace.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
wait:
	for s.sessions.count() > 0 {
		select {
		case <-grace.C:
			break wait
		case <-ctx.Done():
			break wait
		case <-tick.C:
		}
	}
	// Hard stop: cancel sessions and close every remaining connection.
	s.stopAll()
	s.mu.Lock()
	for c := range s.active {
		c.Close()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shutdown: %w", ctx.Err())
	}
}

func (s *Server) sweepLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.hardStop.Done():
			return
		case <-t.C:
			s.admission.Sweep()
		}
	}
}

func (s *Server) statsLoop(iv time.Duration) {
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-s.hardStop.Done():
			return
		case <-t.C:
			s.logStats()
		}
	}
}

func (s *Server) logStats() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	attrs := []any{
		"sessions", s.sessions.count(),
		"connections", s.conns.InUse(),
		"handshakes", s.unauth.InUse(),
		"goroutines", runtime.NumGoroutine(),
		"heap_mb", float64(ms.HeapAlloc) / (1 << 20),
		"sys_mb", float64(ms.Sys) / (1 << 20),
		"tracked_addresses", s.admission.Tracked(),
		"accepted_total", s.stats.accepted.Load(),
		"rejected_total", s.stats.rejected.Load(),
		"auth_failures_total", s.stats.authFailed.Load(),
		"sessions_total", s.stats.sessions.Load(),
		"bytes_in_total", s.stats.bytesIn.Load(),
		"bytes_out_total", s.stats.bytesOut.Load(),
	}
	if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
		attrs = append(attrs, "open_fds", len(fds))
	}
	s.stats.rejectReasons.Range(func(k, v any) bool {
		attrs = append(attrs, "rejected_"+k.(string), v.(*atomic.Int64).Load())
		return true
	})
	s.log.Info("stats", attrs...)
}

// Stats is a snapshot for tests and health checks.
type Stats struct {
	Sessions, Connections, Handshakes int
	Accepted, Rejected, AuthFailed    int64
	BytesIn, BytesOut                 int64
}

// Snapshot returns current counters.
func (s *Server) Snapshot() Stats {
	return Stats{
		Sessions: s.sessions.count(), Connections: s.conns.InUse(), Handshakes: s.unauth.InUse(),
		Accepted: s.stats.accepted.Load(), Rejected: s.stats.rejected.Load(), AuthFailed: s.stats.authFailed.Load(),
		BytesIn: s.stats.bytesIn.Load(), BytesOut: s.stats.bytesOut.Load(),
	}
}

// identity is the authenticated player identity of a connection.
type identity struct {
	kind        string
	key         string // HMAC of the public key; empty for guests
	fingerprint string // SHA256 fingerprint shown to the player
}

func (i identity) guest() bool { return i.kind == kindGuest }

// short is a log-friendly pseudonym that does not reveal the key.
func (i identity) short() string {
	if i.guest() || len(i.key) < 12 {
		return "guest"
	}
	return i.key[:12]
}
