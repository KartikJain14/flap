package sshserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"golang.org/x/crypto/ssh"

	"flap/internal/canvas"
	"flap/internal/config"
	"flap/internal/security"
	"flap/internal/store"
	"flap/internal/tui"
)

var (
	errDisconnected = errors.New("client disconnected")
	errIdle         = errors.New("idle timeout")
	errTimeLimit    = errors.New("session time limit")
	errStalled      = errors.New("client stopped reading output")
)

// allowedEnv are the environment variables a client may pass. They only
// influence colour and glyph detection.
var allowedEnv = map[string]bool{"COLORTERM": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TERM_PROGRAM": true}

type ptyInfo struct {
	term string
	w, h int
}

type startReq struct {
	pty    *ptyInfo
	env    map[string]string
	refuse string // non-empty when the client asked for exec or a subsystem
}

func reply(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

func exitStatus(ch ssh.Channel, code uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}

// sanitizeTerm keeps TERM to a conservative character set.
func sanitizeTerm(t string) string {
	if t == "" || len(t) > 64 {
		return "xterm-256color"
	}
	for _, r := range t {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '+') {
			return "xterm-256color"
		}
	}
	return t
}

func clampSize(w, h uint32, l config.Limits) (int, int) {
	cw, ch := int(min(w, 100000)), int(min(h, 100000))
	if cw == 0 {
		cw = 80
	}
	if ch == 0 {
		ch = 24
	}
	return min(cw, l.MaxTerminalWidth), min(ch, l.MaxTerminalHeight)
}

func parsePty(payload []byte, l config.Limits) (ptyInfo, bool) {
	var p struct {
		Term          string
		Cols, Rows    uint32
		Width, Height uint32
		Modes         string
	}
	if err := ssh.Unmarshal(payload, &p); err != nil {
		return ptyInfo{}, false
	}
	w, h := clampSize(p.Cols, p.Rows, l)
	return ptyInfo{term: sanitizeTerm(p.Term), w: w, h: h}, true
}

func parseWindowChange(payload []byte, l config.Limits) (int, int, bool) {
	var p struct {
		Cols, Rows    uint32
		Width, Height uint32
	}
	if err := ssh.Unmarshal(payload, &p); err != nil {
		return 0, 0, false
	}
	w, h := clampSize(p.Cols, p.Rows, l)
	return w, h, true
}

func parseEnv(payload []byte) (string, string, bool) {
	var p struct{ Name, Value string }
	if err := ssh.Unmarshal(payload, &p); err != nil || len(p.Name) > 32 || len(p.Value) > 64 {
		return "", "", false
	}
	for i := 0; i < len(p.Value); i++ {
		if p.Value[i] < 0x20 || p.Value[i] > 0x7e {
			return "", "", false
		}
	}
	return p.Name, p.Value, true
}

// detectColor guesses the colour support from TERM and COLORTERM. Most SSH
// clients only forward TERM, so 256 colours is the default for the common
// xterm-256color; players can switch with C.
func detectColor(term, colorterm string) canvas.ColorMode {
	ct := strings.ToLower(colorterm)
	if ct == "truecolor" || ct == "24bit" {
		return canvas.TrueColor
	}
	t := strings.ToLower(term)
	for _, p := range []string{"alacritty", "foot", "ghostty", "kitty", "wezterm", "contour", "rio"} {
		if strings.Contains(t, p) {
			return canvas.TrueColor
		}
	}
	switch {
	case strings.Contains(t, "truecolor"), strings.Contains(t, "24bit"), strings.HasSuffix(t, "-direct"):
		return canvas.TrueColor
	case strings.Contains(t, "256"):
		return canvas.Color256
	}
	return canvas.Color16
}

// detectGlyphs picks ASCII for terminals that are unlikely to have the block
// and box-drawing characters, or whose locale is explicitly not UTF-8.
func detectGlyphs(term string, env map[string]string) tui.GlyphMode {
	switch strings.ToLower(term) {
	case "dumb", "vt52", "vt100", "vt102", "vt220", "ansi", "cons25":
		return tui.ASCII
	}
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v, ok := env[k]; ok && v != "" {
			l := strings.ToLower(v)
			if strings.Contains(l, "utf-8") || strings.Contains(l, "utf8") {
				return tui.Unicode
			}
			return tui.ASCII
		}
	}
	return tui.Unicode
}

func (s *Server) displayModes(st startReq, p store.Player) (canvas.ColorMode, tui.GlyphMode, tui.SoundMode) {
	color := detectColor(st.pty.term, st.env["COLORTERM"])
	if m, ok := canvas.ParseColorMode(s.cfg.UI.DefaultColor); ok {
		color = m
	}
	if m, ok := canvas.ParseColorMode(p.ColorMode); ok {
		color = m
	}
	glyphs := detectGlyphs(st.pty.term, st.env)
	if m, ok := tui.ParseGlyphMode(s.cfg.UI.DefaultGlyphs); ok {
		glyphs = m
	}
	if m, ok := tui.ParseGlyphMode(p.GlyphMode); ok {
		glyphs = m
	}
	sound, _ := tui.ParseSoundMode(s.cfg.UI.DefaultSound)
	if m, ok := tui.ParseSoundMode(p.SoundMode); ok {
		sound = m
	}
	return color, glyphs, sound
}

func (s *Server) handleSession(sconn *ssh.ServerConn, ch ssh.Channel, reqs <-chan *ssh.Request, id identity,
	started *atomic.Bool, lastAlive *atomic.Int64, log *slog.Logger) {
	defer ch.Close()
	ctx, cancel := context.WithCancelCause(s.hardStop)
	defer cancel(nil)

	startCh := make(chan startReq, 1)
	var resizeMu sync.Mutex
	var resizeFn func(w, h int)
	// The request loop ends when x/crypto closes reqs: when the peer closes
	// the channel or, at the latest, when the caller closes the connection
	// after this function returns. It never waits on the peer itself, so a
	// client that ignores our channel close cannot pin the session.
	s.wg.Go(func() {
		s.sessionRequests(ch, reqs, startCh, &resizeMu, &resizeFn, log)
		cancel(errDisconnected)
	})

	var st startReq
	timer := time.NewTimer(s.cfg.Limits.SessionStartTimeout.Duration)
	select {
	case st = <-startCh:
		timer.Stop()
	case <-timer.C:
		log.Info("client never requested a shell")
		return
	case <-ctx.Done():
		timer.Stop()
		return
	}
	started.Store(true)
	lastAlive.Store(time.Now().UnixNano())

	say := func(code uint32, lines ...string) {
		_, _ = ch.Write([]byte(strings.Join(lines, "\r\n") + "\r\n"))
		exitStatus(ch, code)
	}
	switch {
	case st.refuse != "":
		log.Info("refused request", "type", st.refuse)
		exitStatus(ch, 1)
		return
	case st.pty == nil:
		log.Info("refused session without a terminal")
		say(1, "flap is a game and needs an interactive terminal.",
			"Connect without a command and allow a terminal, for example: ssh -t -p "+s.port()+" "+s.hostHint())
		return
	case s.cfg.Server.Maintenance:
		say(1, "", "  "+s.cfg.Server.MaintenanceMessage, "")
		return
	}

	e, err := s.sessions.add(id.key)
	if err != nil {
		s.stats.reject(map[error]string{errServerFull: "max_sessions", errTooManyForKey: "max_sessions_per_player"}[err])
		log.Info("session refused", "reason", err.Error())
		if errors.Is(err, errServerFull) {
			say(1, "", "  Flap is full right now. Please try again in a minute.", "")
		} else {
			say(1, "", fmt.Sprintf("  You already have %d games open. Close one and try again.", s.cfg.Limits.MaxSessionsPerPlayer), "")
		}
		return
	}
	defer s.sessions.remove(e)
	s.stats.sessions.Add(1)

	player, resolve, storageDown := s.profile(ctx, id, log)
	color, glyphs, sound := s.displayModes(st, player)
	app := tui.New(s.store, tui.Session{
		Player:         player,
		Guest:          id.guest(),
		KeyFingerprint: id.fingerprint,
		Resolve:        resolve,
		StorageDown:    storageDown,
		Color:          color,
		Glyphs:         glyphs,
		Sound:          sound,
		Width:          st.pty.w,
		Height:         st.pty.h,
	}, tui.Options{
		Params:          s.cfg.GameParams(),
		RenderFPS:       s.cfg.UI.RenderFPS,
		MenuFPS:         s.cfg.UI.MenuFPS,
		MinWidth:        s.cfg.UI.MinWidth,
		MinHeight:       s.cfg.UI.MinHeight,
		LeaderboardSize: s.cfg.Storage.LeaderboardSize,
		StoreTimeout:    s.cfg.Storage.OperationTimeout.Duration,
		ServerName:      s.cfg.Server.Name,
		Online:          s.sessions.count,
		Logger:          log,
	})

	in := newInputReader(ctx, ch, s.cfg.Limits.InputBytesPerSecond, s.cfg.Limits.InputBurstBytes, &s.stats.bytesIn,
		func() { cancel(errDisconnected) })
	out := &outputWriter{w: ch, total: &s.stats.bytesOut}
	env := []string{"TERM=" + st.pty.term, "SSH_TTY=/dev/pts/flap"}
	prog := tea.NewProgram(app,
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithEnvironment(env),
		tea.WithWindowSize(st.pty.w, st.pty.h),
		// The model quantises colours itself; the renderer passes them on.
		tea.WithColorProfile(colorprofile.TrueColor),
		tea.WithoutSignalHandler(),
		tea.WithFPS(s.cfg.UI.RenderFPS),
	)
	e.setProgram(prog)
	resizeMu.Lock()
	resizeFn = func(w, h int) { prog.Send(tea.WindowSizeMsg{Width: w, Height: h}) }
	resizeMu.Unlock()

	runDone := make(chan struct{})
	start := time.Now()
	var endCause atomic.Pointer[error]
	s.wg.Go(func() { s.watchdog(ctx, cancel, e, in, out, sconn, start, runDone, &endCause, log) })

	log.Info("game started", "term", st.pty.term, "size", fmt.Sprintf("%dx%d", st.pty.w, st.pty.h),
		"color", color.String(), "glyphs", glyphs.String(), "sound", sound.String(), "sessions", s.sessions.count())
	_, runErr := prog.Run()
	close(runDone)

	cause := context.Cause(ctx)
	if c := endCause.Load(); c != nil && ctx.Err() == nil {
		// The game wound down by itself after a watchdog notice.
		cause = *c
	}
	reason := store.ReasonDisconnect
	switch {
	case errors.Is(cause, errIdle):
		reason = store.ReasonIdle
	case errors.Is(cause, errTimeLimit):
		reason = store.ReasonTimeLimit
	case s.closing.Load():
		reason = store.ReasonShutdown
	}
	saveCtx, saveCancel := context.WithTimeout(context.Background(), s.cfg.Storage.OperationTimeout.Duration)
	saved, saveErr := app.SaveUnsaved(saveCtx, reason)
	saveCancel()
	if saveErr != nil {
		log.Warn("could not save interrupted run", "err", saveErr)
	}
	if app.Crashed() {
		log.Error("game session crashed and was recovered")
	}

	if ctx.Err() == nil || errors.Is(cause, errIdle) || errors.Is(cause, errTimeLimit) {
		msg := fmt.Sprintf("Thanks for playing Flap, %s! Best score: %d", app.PlayerName(), app.Best())
		switch {
		case errors.Is(cause, errIdle):
			msg = "Disconnected after " + s.cfg.Limits.IdleTimeout.String() + " without input. " + msg
		case errors.Is(cause, errTimeLimit):
			msg = "Session time limit reached. " + msg
		}
		_, _ = ch.Write([]byte("\r\n  " + security.LogString(msg, 160) + "\r\n\r\n"))
	}
	code := uint32(0)
	if runErr != nil && !errors.Is(runErr, tea.ErrProgramKilled) {
		code = 1
	}
	exitStatus(ch, code)
	log.Info("game ended", "duration", time.Since(start).Round(time.Second).String(), "reason", reasonOr(app.ExitReason(), reason),
		"best", app.Best(), "saved_interrupted_run", saved)
}

func reasonOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sessionRequests handles channel requests for the whole session: terminal
// setup before the game starts and window changes while it runs.
func (s *Server) sessionRequests(ch ssh.Channel, reqs <-chan *ssh.Request, startCh chan<- startReq,
	resizeMu *sync.Mutex, resizeFn *func(w, h int), log *slog.Logger) {
	var pty *ptyInfo
	env := map[string]string{}
	startSent := false
	limits := s.cfg.Limits

	// Window changes are applied at most ~10 times a second; a burst is
	// coalesced into its final size so a client cannot force endless
	// full-screen redraws.
	bucket := security.NewTokenBucket(10, 10, time.Now())
	var pendMu sync.Mutex
	var pendW, pendH int
	var pendTimer *time.Timer
	defer func() {
		pendMu.Lock()
		if pendTimer != nil {
			pendTimer.Stop()
		}
		pendMu.Unlock()
	}()
	apply := func(w, h int) {
		resizeMu.Lock()
		fn := *resizeFn
		resizeMu.Unlock()
		if fn != nil {
			fn(w, h)
		}
	}

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			p, ok := parsePty(req.Payload, limits)
			if !ok || startSent {
				reply(req, false)
				continue
			}
			pty = &p
			reply(req, true)
		case "env":
			name, value, ok := parseEnv(req.Payload)
			if ok && allowedEnv[name] && len(env) < len(allowedEnv) && !startSent {
				env[name] = value
				reply(req, true)
			} else {
				reply(req, false)
			}
		case "shell":
			if startSent {
				reply(req, false)
				continue
			}
			startSent = true
			reply(req, true)
			var cp *ptyInfo
			if pty != nil {
				c := *pty
				cp = &c
			}
			startCh <- startReq{pty: cp, env: env}
		case "exec", "subsystem":
			what := "Commands are"
			if req.Type == "subsystem" {
				what = "Subsystems (sftp, scp, ...) are"
			}
			_, _ = ch.Stderr().Write([]byte("flap: " + what + " not supported. Connect without a command to play.\r\n"))
			reply(req, false)
			if !startSent {
				startSent = true
				startCh <- startReq{refuse: req.Type}
			}
		case "window-change":
			w, h, ok := parseWindowChange(req.Payload, limits)
			if !ok {
				reply(req, false)
				continue
			}
			if !startSent {
				if pty != nil {
					pty.w, pty.h = w, h
				}
				continue
			}
			if bucket.Allow(time.Now()) {
				apply(w, h)
				continue
			}
			pendMu.Lock()
			pendW, pendH = w, h
			if pendTimer == nil {
				pendTimer = time.AfterFunc(150*time.Millisecond, func() {
					pendMu.Lock()
					w, h := pendW, pendH
					pendTimer = nil
					pendMu.Unlock()
					apply(w, h)
				})
			}
			pendMu.Unlock()
		default:
			// x11-req, auth-agent-req@openssh.com, signal, break, ...
			if req.Type == "x11-req" || strings.HasPrefix(req.Type, "auth-agent") {
				log.Debug("refused forwarding request", "type", req.Type)
			}
			reply(req, false)
		}
	}
}

// profile loads the player for the session. Storage failures do not prevent
// playing; scores are saved later if storage recovers.
func (s *Server) profile(ctx context.Context, id identity, log *slog.Logger) (store.Player, func(context.Context) (store.Player, error), bool) {
	var mu sync.Mutex
	var cached *store.Player
	memo := func(load func(context.Context) (store.Player, error)) func(context.Context) (store.Player, error) {
		return func(ctx context.Context) (store.Player, error) {
			mu.Lock()
			defer mu.Unlock()
			if cached != nil {
				return *cached, nil
			}
			p, err := load(ctx)
			if err == nil {
				cached = &p
			}
			return p, err
		}
	}
	if id.guest() {
		name := guestName()
		resolve := memo(func(ctx context.Context) (store.Player, error) {
			return s.store.CreateGuest(ctx, func(attempt int) string {
				if attempt == 0 {
					return name
				}
				return guestName()
			})
		})
		return store.Player{Name: name, Kind: store.KindGuest}, resolve, false
	}
	resolve := memo(func(ctx context.Context) (store.Player, error) {
		return s.store.KeyPlayer(ctx, id.key, func(int) string { return playerName() })
	})
	lctx, cancel := context.WithTimeout(ctx, s.cfg.Storage.OperationTimeout.Duration)
	defer cancel()
	p, err := resolve(lctx)
	if err != nil {
		log.Warn("player profile unavailable; scores will be saved if storage recovers", "err", err)
		return store.Player{Name: "pilot", Kind: store.KindKey}, resolve, true
	}
	return p, resolve, false
}

func (s *Server) watchdog(ctx context.Context, cancel context.CancelCauseFunc, e *entry, in *inputReader, out *outputWriter,
	sconn *ssh.ServerConn, start time.Time, runDone <-chan struct{}, endCause *atomic.Pointer[error], log *slog.Logger) {
	l := s.cfg.Limits
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var pending error
	var noticeAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-runDone:
			return
		case <-t.C:
		}
		if d := out.stalled(); d > l.WriteTimeout.Duration {
			log.Info("client stopped reading output; disconnecting", "stalled", d.Round(time.Second).String())
			cancel(errStalled)
			sconn.Close()
			return
		}
		if pending != nil {
			// The game was asked to finish; force it if it has not.
			if time.Since(noticeAt) > 5*time.Second {
				cancel(pending)
				return
			}
			continue
		}
		switch {
		case in.idle() > l.IdleTimeout.Duration:
			pending, noticeAt = errIdle, time.Now()
			endCause.Store(&pending)
			e.send(tui.NoticeMsg{Text: "No input for " + l.IdleTimeout.String() + ". Disconnecting.", Quit: true, Reason: store.ReasonIdle})
		case l.MaxSessionDuration.Duration > 0 && time.Since(start) > l.MaxSessionDuration.Duration:
			pending, noticeAt = errTimeLimit, time.Now()
			endCause.Store(&pending)
			e.send(tui.NoticeMsg{Text: "Session time limit reached. Thanks for playing!", Quit: true, Reason: store.ReasonTimeLimit})
		}
	}
}

var (
	nameAdjectives = []string{"Brave", "Swift", "Lucky", "Sunny", "Misty", "Cosmic", "Pixel", "Turbo", "Breezy", "Plucky",
		"Quiet", "Rapid", "Stormy", "Nimble", "Jolly", "Zesty", "Mellow", "Daring", "Fuzzy", "Golden", "Silver", "Cobalt",
		"Amber", "Velvet", "Tiny", "Mighty", "Clever", "Bouncy", "Dizzy", "Snappy"}
	nameBirds = []string{"Finch", "Robin", "Wren", "Sparrow", "Lark", "Swift", "Kestrel", "Heron", "Puffin", "Plover",
		"Thrush", "Magpie", "Oriole", "Tern", "Petrel", "Falcon", "Owl", "Jay", "Dove", "Hawk", "Egret", "Ibis", "Kiwi",
		"Myna", "Pipit", "Raven", "Rook", "Siskin", "Martin", "Linnet"}
)

// playerName returns a friendly generated display name.
func playerName() string {
	for {
		n := nameAdjectives[rand.IntN(len(nameAdjectives))] + nameBirds[rand.IntN(len(nameBirds))] + fmt.Sprintf("%02d", rand.IntN(100))
		if len(n) <= security.MaxNameLen && security.ValidateName(n) == nil {
			return n
		}
	}
}

// guestName returns a guest name. The alphabet avoids look-alike characters.
func guestName() string {
	const alphabet = "abcdefghkmnpqrstuvwxyz23456789"
	b := []byte("guest-xxxx")
	for i := 6; i < len(b); i++ {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}
