package sshserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"flap/internal/config"
	"flap/internal/game"
	"flap/internal/store"
)

func TestPlayOverSSH(t *testing.T) {
	ts := startServer(t, "", nil)
	tm := ts.open(t, newKey(t), 80, 24)
	tm.waitFor(5*time.Second, "PRESS", "SPACE", "TO PLAY", "Playing as")
	if !tm.altScreen() {
		t.Fatal("game is not using the alternate screen")
	}
	checkCardAligned(t, tm.screen())
	tm.send(" ")
	tm.waitFor(3*time.Second, "BEST", "MORNING")
	// Resize during play pauses the run and redraws at the new size.
	tm.resize(100, 30)
	tm.waitFor(3*time.Second, "RESIZED", "resume")
	tm.send(" ")
	time.Sleep(300 * time.Millisecond)
	tm.send("q")
	if err := tm.waitEnd(5 * time.Second); exitCode(err) != 0 {
		t.Fatalf("exit: %v", err)
	}
	if tm.altScreen() {
		t.Fatal("terminal left in the alternate screen")
	}
	if s := tm.screen(); !strings.Contains(s, "Thanks for playing") {
		t.Fatalf("no farewell on the main screen:\n%s", s)
	}
	waitUntil(t, 3*time.Second, "session cleanup", func() bool { return ts.srv.ActiveSessions() == 0 })
}

// checkCardAligned verifies that the welcome card's frame arrived intact:
// every row between the top and bottom border has its side borders in the
// same columns. Misplaced cursor movement shows up as broken frames.
func checkCardAligned(t *testing.T, screen string) {
	t.Helper()
	rows := strings.Split(screen, "\n")
	top, bottom := -1, -1
	var left, right int
	for i, r := range rows {
		rr := []rune(r)
		if l := strings.IndexRune(r, '╭'); l >= 0 && top < 0 {
			top, left, right = i, runeIndex(rr, '╭'), runeIndex(rr, '╮')
		}
		if strings.ContainsRune(r, '╰') {
			bottom = i
			if runeIndex(rr, '╰') != left || runeIndex(rr, '╯') != right {
				t.Fatalf("bottom border misaligned:\n%s", screen)
			}
		}
	}
	if top < 0 || bottom <= top || right <= left {
		t.Fatalf("card frame not found:\n%s", screen)
	}
	for i := top + 1; i < bottom; i++ {
		rr := []rune(rows[i])
		if len(rr) <= right || rr[left] != '│' || rr[right] != '│' {
			t.Fatalf("row %d of the card is misaligned:\n%s", i, screen)
		}
	}
}

func runeIndex(rs []rune, r rune) int {
	for i, x := range rs {
		if x == r {
			return i
		}
	}
	return -1
}

func TestEscAndCtrlCExit(t *testing.T) {
	ts := startServer(t, "", nil)
	for _, k := range []string{"\x1b", "\x03"} {
		tm := ts.open(t, newKey(t), 80, 24)
		tm.waitFor(5*time.Second, "PRESS")
		tm.send(k)
		if err := tm.waitEnd(5 * time.Second); exitCode(err) != 0 {
			t.Fatalf("key %q: exit %v", k, err)
		}
		if tm.altScreen() {
			t.Fatalf("key %q left the alternate screen on", k)
		}
	}
}

func TestGuestLoginWithoutKey(t *testing.T) {
	ts := startServer(t, "", nil)
	tm := ts.open(t, nil, 80, 24)
	tm.waitFor(5*time.Second, "GUEST", "guest-")
	tm.send("q")
	tm.waitEnd(5 * time.Second)

	pk := startServer(t, "", func(c *config.Config) { c.Auth.Mode = config.AuthPublicKey })
	if _, err := pk.dial(nil); err == nil {
		t.Fatal("publickey mode accepted a keyless client")
	}
	c, err := pk.dial(newKey(t))
	if err != nil {
		t.Fatalf("publickey mode refused a key: %v", err)
	}
	c.Close()
}

func TestAllowlistAndBans(t *testing.T) {
	dir := t.TempDir()
	allowed := newKey(t)
	akf := filepath.Join(dir, "authorized_keys")
	line := "# players\n\n" + string(ssh.MarshalAuthorizedKey(allowed.PublicKey()))
	if err := os.WriteFile(akf, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := startServer(t, dir, func(c *config.Config) {
		c.Auth.Mode = config.AuthAllowlist
		c.Auth.AuthorizedKeysFile = akf
		c.Limits.AuthFailuresBeforeBan = 3
	})
	c, err := ts.dial(allowed)
	if err != nil {
		t.Fatalf("allowed key refused: %v", err)
	}
	c.Close()
	if _, err := ts.dial(nil); err == nil {
		t.Fatal("allowlist mode accepted a keyless client")
	}
	for i := 0; i < 3; i++ {
		if _, err := ts.dial(newKey(t)); err == nil {
			t.Fatal("unlisted key accepted")
		}
	}
	// The address is now banned: even the allowed key is turned away
	// before authentication.
	if _, err := ts.dial(allowed); err == nil {
		t.Fatal("banned address was admitted")
	}
	if ts.srv.Snapshot().AuthFailed < 3 {
		t.Fatalf("auth failures not counted: %+v", ts.srv.Snapshot())
	}
}

func TestCommandsSubsystemsAndForwardingAreRefused(t *testing.T) {
	ts := startServer(t, "", nil)
	c, err := ts.dial(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// exec
	s1, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s1.CombinedOutput("id; cat /etc/passwd"); err == nil || strings.Contains(string(out), "root:") {
		t.Fatalf("exec was not refused: %v %q", err, out)
	}

	c2, _ := ts.dial(newKey(t))
	defer c2.Close()
	s2, _ := c2.NewSession()
	if err := s2.RequestSubsystem("sftp"); err == nil {
		t.Fatal("sftp subsystem accepted")
	}

	c3, _ := ts.dial(newKey(t))
	defer c3.Close()
	if _, err := c3.Dial("tcp", "127.0.0.1:22"); err == nil {
		t.Fatal("direct-tcpip forwarding accepted")
	}
	if _, err := c3.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Fatal("remote port forwarding accepted")
	}
	if _, _, err := c3.OpenChannel("x11", nil); err == nil {
		t.Fatal("x11 channel accepted")
	}
	if _, _, err := c3.OpenChannel("auth-agent@openssh.com", nil); err == nil {
		t.Fatal("agent channel accepted")
	}
	s3, err := c3.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []string{"x11-req", "auth-agent-req@openssh.com", "made-up-request"} {
		if ok, err := s3.SendRequest(req, true, nil); ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v", req, ok, err)
		}
	}
	if ok, _, _ := c3.SendRequest("tcpip-forward", true, ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"0.0.0.0", 8080})); ok {
		t.Fatal("tcpip-forward global request accepted")
	}
	// A second session on the same connection is refused.
	if _, err := c3.NewSession(); err == nil {
		t.Fatal("second session channel accepted")
	}
}

func TestSessionWithoutPTYIsRefused(t *testing.T) {
	ts := startServer(t, "", nil)
	c, err := ts.dial(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	tm := openTerm(t, c, 80, 24, false)
	err = tm.waitEnd(5 * time.Second)
	if exitCode(err) != 1 {
		t.Fatalf("exit %v, want status 1", err)
	}
	if s := tm.screen(); !strings.Contains(s, "interactive terminal") {
		t.Fatalf("no explanation given:\n%s", s)
	}
}

func TestSessionLimits(t *testing.T) {
	ts := startServer(t, "", func(c *config.Config) {
		c.Limits.MaxSessions = 2
		c.Limits.MaxSessionsPerPlayer = 1
	})
	k := newKey(t)
	a := ts.open(t, k, 80, 24)
	a.waitFor(5*time.Second, "PRESS")
	// Same key twice exceeds the per-player limit.
	dup := ts.open(t, k, 80, 24)
	if err := dup.waitEnd(5 * time.Second); exitCode(err) != 1 {
		t.Fatalf("duplicate session exit %v", err)
	}
	if !strings.Contains(dup.screen(), "already have") {
		t.Fatalf("per-player message missing:\n%s", dup.screen())
	}
	b := ts.open(t, newKey(t), 80, 24)
	b.waitFor(5*time.Second, "PRESS")
	full := ts.open(t, newKey(t), 80, 24)
	if err := full.waitEnd(5 * time.Second); exitCode(err) != 1 {
		t.Fatalf("over-limit session exit %v", err)
	}
	if !strings.Contains(full.screen(), "full right now") {
		t.Fatalf("server full message missing:\n%s", full.screen())
	}
	if n := ts.srv.ActiveSessions(); n != 2 {
		t.Fatalf("%d active sessions, want 2", n)
	}
	a.send("q")
	a.waitEnd(5 * time.Second)
	waitUntil(t, 3*time.Second, "slot release", func() bool { return ts.srv.ActiveSessions() == 1 })
	again := ts.open(t, newKey(t), 80, 24)
	again.waitFor(5*time.Second, "PRESS")
}

func TestConnectionAdmissionLimits(t *testing.T) {
	ts := startServer(t, "", func(c *config.Config) {
		c.Limits.MaxUnauthenticated = 2
		c.Limits.HandshakeTimeout = config.D(2 * time.Second)
	})
	// Two silent TCP connections occupy every handshake slot.
	var silent []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatal(err)
		}
		silent = append(silent, c)
	}
	waitUntil(t, 2*time.Second, "handshake slots in use", func() bool { return ts.srv.Snapshot().Handshakes == 2 })
	if c, err := ts.dial(newKey(t)); err == nil {
		c.Close()
		t.Fatal("connection admitted while all handshake slots were taken")
	}
	// The handshake timeout frees the slots.
	start := time.Now()
	for _, c := range silent {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 512)
		for {
			if _, err := c.Read(buf); err != nil {
				break
			}
		}
		c.Close()
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("silent connections lived %v, longer than the handshake timeout", d)
	}
	waitUntil(t, 2*time.Second, "slots released", func() bool { return ts.srv.Snapshot().Handshakes == 0 })
	c, err := ts.dial(newKey(t))
	if err != nil {
		t.Fatalf("not admitted after slots freed: %v", err)
	}
	c.Close()

	// Per-IP connection rate limiting.
	rl := startServer(t, "", func(c *config.Config) {
		c.Limits.ConnectionRatePerIP = 1
		c.Limits.ConnectionBurstPerIP = 3
	})
	ok := 0
	for i := 0; i < 6; i++ {
		if c, err := rl.dial(newKey(t)); err == nil {
			ok++
			c.Close()
		}
	}
	if ok != 3 {
		t.Fatalf("%d connections admitted, want the burst of 3", ok)
	}
	if rl.srv.Snapshot().Rejected < 3 {
		t.Fatalf("rejections not counted: %+v", rl.srv.Snapshot())
	}
}

// easyGame makes scoring predictable for an open-loop client that flaps at a
// fixed rhythm: a huge, centred gap and no vertical shift.
func easyGame(c *config.Config) {
	c.Game.FirstPipeDistance = 0.5
	c.Game.Stages = []game.Stage{{MinScore: 0, Speed: 0.5, Gap: 0.7, Spacing: 0.6, GapShift: 0}}
}

// hover flaps at the rhythm that keeps the bird level until stop is closed.
func hover(tm *term, stop <-chan struct{}) {
	p := game.DefaultParams()
	period := time.Duration(2 * p.FlapVelocity / p.Gravity * float64(time.Second))
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			tm.send(" ")
		}
	}
}

func TestScoresSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	key := newKey(t)
	ts := startServer(t, dir, easyGame)
	tm := ts.open(t, key, 80, 24)
	tm.waitFor(5*time.Second, "PRESS")
	tm.send(" ")
	stop := make(chan struct{})
	go hover(tm, stop)
	time.Sleep(5 * time.Second)
	close(stop)
	tm.send("q")
	tm.waitEnd(5 * time.Second)
	waitUntil(t, 3*time.Second, "session end", func() bool { return ts.srv.ActiveSessions() == 0 })

	page, err := ts.st.Leaderboard(context.Background(), 0, 10)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Score < 2 {
		t.Fatalf("score not stored before restart: %+v (%v)", page, err)
	}
	best := page.Entries[0].Score
	ts.stop()

	// Restart with the same data directory: same host key, same secret.
	again := startServer(t, dir, easyGame)
	if again.hostKey.PublicKey().Type() != ts.hostKey.PublicKey().Type() ||
		string(again.hostKey.PublicKey().Marshal()) != string(ts.hostKey.PublicKey().Marshal()) {
		t.Fatal("host key changed across restart")
	}
	tm2 := again.open(t, key, 80, 24)
	tm2.waitFor(5*time.Second, fmt.Sprintf("BEST %d", best), "#1 of 1")
	tm2.send("q")
	tm2.waitEnd(5 * time.Second)
}

func TestConcurrentSessionsAreIsolated(t *testing.T) {
	baseline := runtime.NumGoroutine()
	ts := startServer(t, "", easyGame)
	const n = 6
	terms := make([]*term, n)
	for i := range terms {
		terms[i] = ts.open(t, newKey(t), 60+i*10, 20+i*2)
	}
	for _, tm := range terms {
		tm.waitFor(5*time.Second, "PRESS")
	}
	if got := ts.srv.ActiveSessions(); got != n {
		t.Fatalf("%d sessions, want %d", got, n)
	}
	// Half the players start; the others stay on the menu.
	for i := 0; i < n; i += 2 {
		terms[i].send(" ")
	}
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			terms[i].waitFor(3*time.Second, "MORNING")
		} else if s := terms[i].screen(); !strings.Contains(s, "PRESS") {
			t.Fatalf("idle player %d affected by others:\n%s", i, s)
		}
	}
	// One player vanishes without a goodbye (TCP drop).
	terms[0].client.Close()
	waitUntil(t, 5*time.Second, "dropped session cleanup", func() bool { return ts.srv.ActiveSessions() == n-1 })
	terms[1].send(" ")
	terms[1].waitFor(3*time.Second, "MORNING")
	for i := 1; i < n; i++ {
		terms[i].send("q")
	}
	for i := 1; i < n; i++ {
		terms[i].waitEnd(5 * time.Second)
	}
	waitUntil(t, 5*time.Second, "all sessions closed", func() bool { return ts.srv.ActiveSessions() == 0 })
	ts.stop()
	for _, tm := range terms {
		tm.client.Close()
	}
	waitUntil(t, 10*time.Second, "goroutines to drain", func() bool {
		return runtime.NumGoroutine() <= baseline+5
	})
}

func TestAbruptDisconnectSavesRun(t *testing.T) {
	ts := startServer(t, "", easyGame)
	tm := ts.open(t, newKey(t), 80, 24)
	tm.waitFor(5*time.Second, "PRESS")
	tm.send(" ")
	stop := make(chan struct{})
	go hover(tm, stop)
	time.Sleep(4 * time.Second)
	close(stop)
	tm.client.Close() // no quit key, the connection just dies
	waitUntil(t, 5*time.Second, "session end", func() bool { return ts.srv.ActiveSessions() == 0 })
	var reason string
	var score int
	err := ts.st.DB().QueryRow(`SELECT reason, score FROM runs ORDER BY id DESC LIMIT 1`).Scan(&reason, &score)
	if err != nil {
		t.Fatalf("no run stored: %v", err)
	}
	if reason != store.ReasonDisconnect || score < 1 {
		t.Fatalf("stored run reason=%q score=%d", reason, score)
	}
}

func TestGracefulShutdownNotifiesPlayers(t *testing.T) {
	ts := startServer(t, "", easyGame)
	tm := ts.open(t, newKey(t), 80, 24)
	tm.waitFor(5*time.Second, "PRESS")
	tm.send(" ")
	stop := make(chan struct{})
	go hover(tm, stop)
	time.Sleep(3500 * time.Millisecond)
	done := make(chan struct{})
	go func() { ts.stop(); close(done) }()
	tm.waitFor(3*time.Second, "restarting")
	close(stop)
	tm.waitEnd(5 * time.Second)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if tm.altScreen() {
		t.Fatal("terminal left in the alternate screen after shutdown")
	}
	st, err := store.Open(context.Background(), ts.cfg.Storage.Path, store.Options{IntegrityCheck: "full"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var reason string
	if err := st.DB().QueryRow(`SELECT reason FROM runs ORDER BY id DESC LIMIT 1`).Scan(&reason); err != nil || reason != store.ReasonShutdown {
		t.Fatalf("run not saved on shutdown: %q %v", reason, err)
	}
}

func TestIdleTimeoutDisconnects(t *testing.T) {
	ts := startServer(t, "", func(c *config.Config) { c.Limits.IdleTimeout = config.D(5 * time.Second) })
	tm := ts.open(t, newKey(t), 80, 24)
	tm.waitFor(5*time.Second, "PRESS")
	start := time.Now()
	if err := tm.waitEnd(15 * time.Second); exitCode(err) != 0 {
		t.Fatalf("exit %v", err)
	}
	if d := time.Since(start); d < 4*time.Second || d > 12*time.Second {
		t.Fatalf("idle session ended after %v", d)
	}
	if s := tm.screen(); !strings.Contains(s, "without input") {
		t.Fatalf("no idle explanation:\n%s", s)
	}
}

func TestStalledClientIsDisconnected(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	ts := startServer(t, "", func(c *config.Config) {
		easyGame(c)
		c.Limits.WriteTimeout = config.D(2 * time.Second)
		c.UI.MenuFPS = 60
		c.UI.RenderFPS = 60
	})
	c, err := ts.dial(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, _ := c.NewSession()
	sess.RequestPty("xterm-256color", 200, 400, nil)
	stdin, _ := sess.StdinPipe()
	// Hold the output pipe but never read it: the SSH window fills up and
	// the server's writes block.
	if _, err := sess.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	sess.Shell()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// Keep the game animating so there is output to send.
		tk := time.NewTicker(590 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				stdin.Write([]byte(" "))
			}
		}
	}()
	start := time.Now()
	waitUntil(t, 60*time.Second, "stalled client to be dropped", func() bool {
		return ts.srv.ActiveSessions() == 0 && ts.srv.Snapshot().Connections == 0
	})
	t.Logf("stalled client dropped after %v", time.Since(start).Round(100*time.Millisecond))
}

func TestMalformedRequestsAreHandled(t *testing.T) {
	ts := startServer(t, "", nil)
	c, err := ts.dial(newKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe() // keep stdin open; EOF on stdin ends the game
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if ok, _ := sess.SendRequest("pty-req", true, []byte{1, 2, 3}); ok {
		t.Fatal("garbage pty-req accepted")
	}
	if ok, _ := sess.SendRequest("env", true, []byte("garbage")); ok {
		t.Fatal("garbage env accepted")
	}
	if ok, _ := sess.SendRequest("env", true, ssh.Marshal(struct{ Name, Value string }{"LD_PRELOAD", "/tmp/x.so"})); ok {
		t.Fatal("dangerous env accepted")
	}
	// A huge PTY is clamped instead of allocating gigabytes.
	huge := ssh.Marshal(struct {
		Term                      string
		Cols, Rows, Width, Height uint32
		Modes                     string
	}{"xterm\x1b[31m", 1 << 30, 1 << 30, 0, 0, ""})
	if ok, _ := sess.SendRequest("pty-req", true, huge); !ok {
		t.Fatal("pty-req with huge size refused instead of clamped")
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	sess.SendRequest("window-change", false, []byte{9})
	for i := 0; i < 200; i++ {
		sess.SendRequest("window-change", false, ssh.Marshal(struct{ C, R, W, H uint32 }{uint32(20 + i%300), uint32(5 + i%90), 0, 0}))
	}
	time.Sleep(500 * time.Millisecond)
	if ts.srv.ActiveSessions() != 1 {
		t.Fatal("session died from malformed requests")
	}
	sess.Close()
	waitUntil(t, 5*time.Second, "cleanup", func() bool { return ts.srv.ActiveSessions() == 0 })
}

func TestParseHelpers(t *testing.T) {
	l := config.Default().Limits
	if w, h := clampSize(0, 0, l); w != 80 || h != 24 {
		t.Fatalf("zero size -> %dx%d", w, h)
	}
	if w, h := clampSize(1<<31, 1<<31, l); w != l.MaxTerminalWidth || h != l.MaxTerminalHeight {
		t.Fatalf("huge size -> %dx%d", w, h)
	}
	for in, want := range map[string]string{
		"xterm-256color": "xterm-256color", "": "xterm-256color", "a b": "xterm-256color",
		"\x1b]0;x": "xterm-256color", strings.Repeat("x", 100): "xterm-256color", "screen.xterm-256color": "screen.xterm-256color",
	} {
		if got := sanitizeTerm(in); got != want {
			t.Errorf("sanitizeTerm(%q) = %q", in, got)
		}
	}
	if detectColor("xterm-256color", "") != 1 || detectColor("xterm-kitty", "") != 0 || detectColor("xterm", "truecolor") != 0 || detectColor("vt100", "") != 2 {
		t.Error("color detection")
	}
	if detectGlyphs("vt100", nil) != 1 || detectGlyphs("xterm", map[string]string{"LANG": "C"}) != 1 || detectGlyphs("xterm", map[string]string{"LANG": "en_US.UTF-8"}) != 0 {
		t.Error("glyph detection")
	}
	for i := 0; i < 1000; i++ {
		if n := playerName(); len(n) > 16 {
			t.Fatalf("generated name %q too long", n)
		}
		if g := guestName(); !strings.HasPrefix(g, "guest-") || len(g) != 10 {
			t.Fatalf("guest name %q", g)
		}
	}
}

func TestHostKeysAndSecretPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	k1, gen, err := LoadHostKeys([]string{path}, true)
	if err != nil || len(gen) != 1 {
		t.Fatalf("generate: %v %v", gen, err)
	}
	k2, gen, err := LoadHostKeys([]string{path}, true)
	if err != nil || len(gen) != 0 {
		t.Fatalf("reload: %v %v", gen, err)
	}
	if string(k1[0].PublicKey().Marshal()) != string(k2[0].PublicKey().Marshal()) {
		t.Fatal("host key changed on reload")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("host key mode %v", st.Mode().Perm())
	}
	if _, _, err := LoadHostKeys([]string{filepath.Join(dir, "missing")}, false); err == nil || !strings.Contains(err.Error(), "keygen") {
		t.Fatalf("missing key without generation: %v", err)
	}
	os.Chmod(path, 0o644)
	if _, _, err := LoadHostKeys([]string{path}, true); err == nil {
		t.Fatal("world-readable host key accepted")
	}
	sp := filepath.Join(dir, "secret")
	s1, created, err := LoadSecret(sp)
	if err != nil || !created || len(s1) != 32 {
		t.Fatalf("secret: %v %v", created, err)
	}
	s2, created, _ := LoadSecret(sp)
	if created || string(s1) != string(s2) {
		t.Fatal("secret not persisted")
	}
}

func TestIdentityIsKeyedAndStable(t *testing.T) {
	ts := startServer(t, "", nil)
	k := newKey(t)
	id1 := ts.srv.identity(k.PublicKey())
	if id1 != ts.srv.identity(k.PublicKey()) || len(id1) != 64 {
		t.Fatal("identity not stable")
	}
	if id1 == ts.srv.identity(newKey(t).PublicKey()) {
		t.Fatal("different keys share an identity")
	}
	if strings.Contains(id1, strings.TrimPrefix(ssh.FingerprintSHA256(k.PublicKey()), "SHA256:")) {
		t.Fatal("identity exposes the public fingerprint")
	}
	other := &Server{secret: []byte("another secret of at least 32 bytes!!")}
	if other.identity(k.PublicKey()) == id1 {
		t.Fatal("identity does not depend on the server secret")
	}
	// Same key, two connections, one player.
	for i := 0; i < 2; i++ {
		tm := ts.open(t, k, 80, 24)
		tm.waitFor(5*time.Second, "PRESS")
		tm.send("q")
		tm.waitEnd(5 * time.Second)
	}
	players, _, _ := ts.st.Stats(context.Background())
	if players != 1 {
		t.Fatalf("%d players for one key", players)
	}
}

func TestWeakClientKeysRejected(t *testing.T) {
	if err := acceptableKey(newKey(t).PublicKey()); err != nil {
		t.Fatal(err)
	}
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ssh.NewPublicKey(&weak.PublicKey)
	if err := acceptableKey(pub); err == nil {
		t.Fatal("1024-bit RSA key accepted")
	}
	strong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ = ssh.NewPublicKey(&strong.PublicKey)
	if err := acceptableKey(pub); err != nil {
		t.Fatalf("2048-bit RSA key rejected: %v", err)
	}
}
