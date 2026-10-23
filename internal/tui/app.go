// Package tui implements the Flap terminal user interface as a Bubble Tea
// model: screens, layout, input handling and rendering.
//
// The model never trusts the client with anything but key presses. Scores
// come from the server-side simulation in package game and are persisted
// through the Store interface.
package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"runtime/debug"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"flap/internal/canvas"
	"flap/internal/game"
	"flap/internal/security"
	"flap/internal/store"
)

// GlyphMode selects the character repertoire.
type GlyphMode uint8

const (
	// Unicode uses half blocks and box drawing characters.
	Unicode GlyphMode = iota
	// ASCII uses only ASCII characters; colour still comes from SGR.
	ASCII
)

func (g GlyphMode) String() string {
	if g == ASCII {
		return "ascii"
	}
	return "unicode"
}

// ParseGlyphMode parses the String form of a GlyphMode.
func ParseGlyphMode(s string) (GlyphMode, bool) {
	switch s {
	case "unicode":
		return Unicode, true
	case "ascii":
		return ASCII, true
	}
	return Unicode, false
}

// SoundMode selects whether sound effects play.
type SoundMode uint8

const (
	// SoundOn rings the terminal bell on speed-ups, new bests and crashes.
	SoundOn SoundMode = iota
	// SoundOff is silent.
	SoundOff
)

func (s SoundMode) String() string {
	if s == SoundOff {
		return "off"
	}
	return "on"
}

// ParseSoundMode parses the String form of a SoundMode.
func ParseSoundMode(s string) (SoundMode, bool) {
	switch s {
	case "on":
		return SoundOn, true
	case "off":
		return SoundOff, true
	}
	return SoundOn, false
}

// bell is the terminal bell (BEL). It is the only sound every terminal can
// make on every platform: Windows Terminal, the Windows console, Linux and
// macOS terminals all ring (or flash, if configured) on it. It is a single
// control byte that does not move the cursor or change colours, so it can
// be written between frames safely.
const bell = "\a"

// Store is the persistence the UI depends on. *store.Store implements it.
type Store interface {
	RecordRun(ctx context.Context, playerID int64, r store.Run) (store.RunResult, error)
	Leaderboard(ctx context.Context, offset, limit int) (store.Page, error)
	Rank(ctx context.Context, playerID int64) (rank, total int, err error)
	RecentRuns(ctx context.Context, playerID int64, limit int) ([]store.RunSummary, error)
	Rename(ctx context.Context, playerID int64, name string) error
	SetPrefs(ctx context.Context, playerID int64, p store.Prefs) error
}

// Session describes the connected player.
type Session struct {
	// Player is the stored profile. Its ID is zero for a guest who has not
	// saved a run yet, or when storage was unavailable at connect time.
	Player store.Player
	Guest  bool
	// KeyFingerprint is shown in the help screen so players can tell which
	// key their scores belong to.
	KeyFingerprint string
	// Resolve loads or creates the stored profile when Player.ID is zero.
	Resolve func(ctx context.Context) (store.Player, error)
	// StorageDown is set when the profile could not be loaded at connect.
	StorageDown bool
	Color       canvas.ColorMode
	Glyphs      GlyphMode
	Sound       SoundMode
	Width       int
	Height      int
}

// Options configures the UI.
type Options struct {
	Params          game.Params
	RenderFPS       int
	MenuFPS         int
	MinWidth        int
	MinHeight       int
	LeaderboardSize int
	StoreTimeout    time.Duration
	ServerName      string
	// Online reports how many games are running; nil hides the indicator.
	Online func() int
	// Seed returns the seed for a new run; nil picks a random one.
	Seed func() uint64
	// Now is the wall clock used for timestamps; nil uses time.Now.
	Now    func() time.Time
	Logger *slog.Logger
	// NoTicks disables the internal frame timer, for tests that drive the
	// model with explicit TickMsg values.
	NoTicks bool
}

type screen uint8

const (
	screenWelcome screen = iota
	screenPlay
	screenOver
	screenBoard
)

type overlay uint8

const (
	overlayNone overlay = iota
	overlayHelp
	overlayName
)

type saveState uint8

const (
	saveNone saveState = iota
	saveBusy
	saveRetrying
	saveOK
	saveFailed
	saveSkipped
)

// Messages.
type (
	// TickMsg advances animation and simulation to the given time.
	TickMsg struct{ At time.Time }
	// NoticeMsg is sent by the server, for example before a restart.
	NoticeMsg struct {
		Text   string
		Quit   bool
		Reason string
	}
	savedMsg struct {
		run     store.Run
		res     store.RunResult
		player  *store.Player
		err     error
		attempt int
	}
	retrySaveMsg struct {
		run     store.Run
		attempt int
	}
	topMsg struct {
		page   store.Page
		rank   int
		ranked int
		err    error
	}
	boardMsg struct {
		offset int
		page   store.Page
		rank   int
		ranked int
		recent []store.RunSummary
		err    error
	}
	renamedMsg struct {
		name string
		err  error
	}
	prefsMsg struct{ err error }
	quitMsg  struct{}
)

var errNoStorage = errors.New("storage unavailable")

// App is the Bubble Tea model for one player session.
type App struct {
	opt  Options
	st   Store
	sess Session
	log  *slog.Logger
	now  func() time.Time
	rng  *rand.Rand

	w, h   int
	lay    layout
	color  canvas.ColorMode
	glyphs GlyphMode
	sound  SoundMode
	g      glyphSet

	screen     screen
	prevScreen screen
	overlay    overlay

	// Current run.
	world      *game.World
	runID      string
	paused     bool
	pauseWhy   string
	countdown  float64
	acc        float64
	overAt     float64
	runBest    bool // passed the personal best during this run
	birdAngle  float64
	exitReason string

	// Last finished run and its save status.
	lastRun   store.Run
	lastCrash game.Crash
	save      saveState
	saveErr   string
	newRecord bool

	// Profile and leaderboard caches.
	best, rank, ranked int
	top                []store.Entry
	topLoading         bool
	topErr             bool
	board              []store.Entry
	boardTotal         int
	boardPage          int
	boardLoading       bool
	boardErr           bool
	recent             []store.RunSummary
	sessionBest        int
	runs               int

	// Name editing.
	nameInput string
	nameErr   string
	nameBusy  bool

	// Preferences persistence.
	prefsDirty, prefsBusy bool

	// Sound effects queued during an update, sent with its commands.
	soundOut strings.Builder
	lastBell float64

	// Presentation.
	t          float64
	lastTick   time.Time
	menuScroll float64
	themeFrom  theme
	themeTo    theme
	themeT     float64
	fx         particles
	scoreFlash float64
	banner     string
	bannerSub  string
	bannerT    float64
	notice     string
	quitAt     float64
	crashed    bool

	scene *scene
	cv    *canvas.Canvas
	buf   bytes.Buffer
	cache string
	dirty bool
}

// New builds the model for a session.
func New(st Store, sess Session, opt Options) *App {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Seed == nil {
		opt.Seed = rand.Uint64
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.RenderFPS <= 0 {
		opt.RenderFPS = 30
	}
	if opt.MenuFPS <= 0 {
		opt.MenuFPS = 12
	}
	if opt.StoreTimeout <= 0 {
		opt.StoreTimeout = 3 * time.Second
	}
	if opt.LeaderboardSize <= 0 {
		opt.LeaderboardSize = 100
	}
	a := &App{
		opt:    opt,
		st:     st,
		sess:   sess,
		log:    opt.Logger,
		now:    opt.Now,
		rng:    rand.New(rand.NewPCG(opt.Seed(), 0x5eed)),
		color:  sess.Color,
		sound:  sess.Sound,
		glyphs: sess.Glyphs,
		scene:  newScene(),
		cv:     canvas.New(0, 0),
		dirty:  true,
		best:   sess.Player.Best,
		runs:   sess.Player.Runs,
	}
	a.themeFrom, a.themeTo, a.themeT = themes[0], themes[0], 1
	a.lastBell = -1
	a.g = glyphsFor(a.glyphs)
	a.resize(sess.Width, sess.Height)
	return a
}

// Init implements tea.Model.
func (a *App) Init() tea.Cmd {
	return tea.Batch(a.scheduleTick(), a.loadTop())
}

func (a *App) skinIndex() int {
	id := a.sess.Player.ID
	if id <= 0 {
		return 0
	}
	return int(id % int64(len(skins)))
}

func (a *App) currentTheme() theme { return blendTheme(a.themeFrom, a.themeTo, a.themeT) }

func (a *App) setTheme(t theme, instant bool) {
	if instant {
		a.themeFrom, a.themeTo, a.themeT = t, t, 1
		return
	}
	a.themeFrom, a.themeTo, a.themeT = a.currentTheme(), t, 0
}

func (a *App) resize(w, h int) {
	w, h = max(w, 1), max(h, 1)
	changed := w != a.w || h != a.h
	a.w, a.h = w, h
	a.lay = computeLayout(w, h, a.opt.MinWidth, a.opt.MinHeight, a.glyphs == ASCII)
	if a.world != nil {
		a.world.SetViewWidth(math.Max(a.lay.viewW, minViewWidth))
	}
	a.fx.list = a.fx.list[:0]
	if changed && a.screen == screenPlay && a.world != nil && a.world.Phase() == game.Playing && !a.paused {
		a.pause("Resized")
	}
	a.dirty = true
}

func (a *App) pause(why string) {
	a.paused = true
	a.pauseWhy = why
	a.countdown = 0
}

func (a *App) resume() {
	a.paused = false
	a.countdown = 1.5
	a.acc = 0
}

// PlayFPS returns the gameplay frame rate for a terminal size. Every frame
// rewrites the rows that scrolling pipes cover, so the cost of a frame grows
// with the terminal area. Large terminals get a somewhat lower frame rate
// (never below 18) to keep each session's bandwidth roughly constant. The
// simulation always runs at its own fixed rate, so gameplay is unaffected.
func PlayFPS(maxFPS, w, h int) int {
	const refArea = 80 * 24
	fps := maxFPS
	if area := w * h; area > refArea {
		fps = int(math.Round(float64(maxFPS) * math.Sqrt(float64(refArea)/float64(area))))
	}
	return max(min(18, maxFPS), min(fps, maxFPS))
}

func (a *App) frameInterval() time.Duration {
	fps := a.opt.MenuFPS
	if a.screen == screenPlay && !a.paused && a.overlay == overlayNone && !a.lay.tooSmall {
		fps = PlayFPS(a.opt.RenderFPS, a.w, a.h)
	}
	return time.Second / time.Duration(fps)
}

// after delivers msg once d has passed. With NoTicks (tests) it is delivered
// immediately.
func (a *App) after(d time.Duration, msg tea.Msg) tea.Cmd {
	if a.opt.NoTicks {
		return func() tea.Msg { return msg }
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

func (a *App) scheduleTick() tea.Cmd {
	if a.opt.NoTicks {
		return nil
	}
	return tea.Tick(a.frameInterval(), func(t time.Time) tea.Msg { return TickMsg{At: t} })
}

// Update implements tea.Model.
func (a *App) Update(msg tea.Msg) (m tea.Model, cmd tea.Cmd) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("panic in game update", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			a.crashed = true
			m, cmd = a, tea.Quit
		}
	}()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.resize(msg.Width, msg.Height)
	case TickMsg:
		return a, a.flushSound(a.tick(msg.At))
	case tea.KeyPressMsg:
		return a, a.flushSound(a.key(msg))
	case savedMsg:
		return a, a.onSaved(msg)
	case retrySaveMsg:
		if msg.run.UUID == a.lastRun.UUID && a.save == saveRetrying {
			return a, a.saveCmd(msg.run, msg.attempt)
		}
	case topMsg:
		a.topLoading = false
		a.topErr = msg.err != nil
		if msg.err == nil {
			a.top = msg.page.Entries
			if msg.rank > 0 || msg.ranked > 0 {
				a.rank, a.ranked = msg.rank, msg.ranked
			}
		}
		a.dirty = true
	case boardMsg:
		a.onBoard(msg)
	case renamedMsg:
		a.onRenamed(msg)
	case prefsMsg:
		a.prefsBusy = false
		if msg.err != nil {
			a.log.Warn("saving display preferences failed", "err", msg.err)
		}
		if a.prefsDirty {
			return a, a.savePrefs()
		}
	case NoticeMsg:
		a.notice = security.LogString(msg.Text, 120)
		if msg.Reason != "" {
			a.exitReason = msg.Reason
		}
		if msg.Quit {
			a.quitAt = a.t + 1.5
			if a.screen == screenPlay && a.world != nil && a.world.Phase() == game.Playing {
				a.pause("Server notice")
			}
		}
		a.dirty = true
	case quitMsg:
		return a, tea.Quit
	}
	return a, nil
}

// View implements tea.Model.
func (a *App) View() (v tea.View) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("panic in game view", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			a.crashed = true
			v = tea.NewView("")
			v.AltScreen = true
		}
	}()
	if a.dirty || a.cache == "" {
		a.render()
		a.dirty = false
	}
	v = tea.NewView(a.cache)
	v.AltScreen = true
	v.DisableBracketedPasteMode = true
	return v
}

// Crashed reports whether the model recovered from a panic.
func (a *App) Crashed() bool { return a.crashed }

// Best returns the best score known for the player.
func (a *App) Best() int { return max(a.best, a.sessionBest) }

// PlayerName returns the player's display name.
func (a *App) PlayerName() string { return a.sess.Player.Name }

// ExitReason returns why the player left, if the model knows.
func (a *App) ExitReason() string { return a.exitReason }

func (a *App) tick(at time.Time) tea.Cmd {
	var dt float64
	if !a.lastTick.IsZero() {
		dt = at.Sub(a.lastTick).Seconds()
	}
	a.lastTick = at
	dt = math.Max(0, math.Min(dt, 0.25))
	a.t += dt
	a.dirty = true
	if a.themeT < 1 {
		a.themeT = math.Min(1, a.themeT+dt/2.5)
	}
	a.fx.update(dt)
	a.scoreFlash = math.Max(0, a.scoreFlash-dt*3)
	if a.bannerT > 0 {
		a.bannerT = math.Max(0, a.bannerT-dt)
	}
	switch a.screen {
	case screenPlay:
		a.tickPlay(dt)
		if a.world.Phase() == game.Over && a.t-a.overAt > 0.5 {
			cmd := a.finishRun()
			return tea.Batch(cmd, a.scheduleTick())
		}
	case screenWelcome:
		a.menuScroll += dt * 0.12
	case screenOver, screenBoard:
		if a.newRecord && a.screen == screenOver && a.t-a.overAt < 3.5 && a.rng.Float64() < 0.6 {
			a.confetti()
		}
	}
	if a.quitAt > 0 && a.t >= a.quitAt {
		return tea.Quit
	}
	return a.scheduleTick()
}

func (a *App) tickPlay(dt float64) {
	w := a.world
	if a.lay.tooSmall || a.paused || a.overlay != overlayNone || a.quitAt > 0 {
		return
	}
	if a.countdown > 0 {
		a.countdown = math.Max(0, a.countdown-dt)
		return
	}
	step := a.opt.Params.Dt()
	a.acc += dt
	steps := 0
	for a.acc >= step && steps < 12 {
		w.Step()
		a.acc -= step
		steps++
		a.handleEvents()
	}
	if steps == 12 {
		// The server fell behind; drop the backlog instead of fast-forwarding
		// the bird into a pipe.
		a.acc = 0
	}
	// Visual tilt follows the vertical speed.
	b := w.Bird()
	target := 0.0
	if b.VY < 0 {
		target = -22 * math.Min(1, -b.VY/a.opt.Params.FlapVelocity)
	} else {
		target = 75 * math.Min(1, math.Max(0, b.VY-0.25)/(a.opt.Params.MaxFallSpeed-0.25))
	}
	if w.Phase() != game.Playing {
		target = 85
	}
	a.birdAngle += (target - a.birdAngle) * math.Min(1, dt*14)
}

func (a *App) birdPixel() (x, y float64) {
	return a.lay.worldX(a.opt.Params.BirdX), a.lay.worldY(a.world.Bird().Y)
}

func (a *App) handleEvents() {
	for _, e := range a.world.Events() {
		switch e.Kind {
		case game.EventFlap:
			x, y := a.birdPixel()
			for i := 0; i < 2; i++ {
				a.fx.add(particle{x: x - 3, y: y + 1, vx: -20 - a.rng.Float64()*15, vy: 6 + a.rng.Float64()*8, ttl: 0.25, col: hex(0xffffff)})
			}
		case game.EventScore:
			a.scoreFlash = 1
			x, y := a.birdPixel()
			for i := 0; i < 6; i++ {
				ang := a.rng.Float64() * 2 * math.Pi
				a.fx.add(particle{x: x, y: y, vx: math.Cos(ang) * 30, vy: math.Sin(ang) * 20, ttl: 0.4, col: hex(0xfff1a0)})
			}
			if !a.runBest && a.Best() > 0 && e.Score > a.Best() {
				a.runBest = true
				a.showBanner("NEW BEST", "keep going!")
				a.ringBell()
			}
		case game.EventStage:
			th := themeFor(e.Stage)
			a.setTheme(th, false)
			a.showBanner(strings.ToUpper(th.name), fmt.Sprintf("faster: speed %d%%", a.speedPercent()))
			a.ringBell()
		case game.EventCrash:
			a.ringBell()
			x, y := a.birdPixel()
			sk := skins[a.skinIndex()]
			for i := 0; i < 14; i++ {
				ang := a.rng.Float64() * 2 * math.Pi
				col := sk.body
				if i%3 == 0 {
					col = sk.wing
				}
				a.fx.add(particle{x: x, y: y, vx: math.Cos(ang) * 45, vy: math.Sin(ang)*30 - 10, gravity: 60, ttl: 0.7, col: col})
			}
		case game.EventLanded:
			a.overAt = a.t
		}
	}
}

func (a *App) showBanner(title, sub string) {
	a.banner, a.bannerSub, a.bannerT = title, sub, 2.2
}

func (a *App) confetti() {
	cols := []rgb{colGold, colAccent2, colGood, colBad, hex(0xff8fd8), hex(0xffffff)}
	a.fx.add(particle{
		x: a.rng.Float64() * float64(a.lay.pxW()), y: -1,
		vx: (a.rng.Float64() - 0.5) * 10, vy: 12 + a.rng.Float64()*14, gravity: 4,
		ttl: 2.5, col: cols[a.rng.IntN(len(cols))],
	})
}

func (a *App) startRun() {
	seed := a.opt.Seed()
	a.world = game.NewWorld(a.opt.Params, seed, math.Max(a.lay.viewW, minViewWidth))
	a.world.Flap()
	a.runID = store.NewRunID()
	a.screen = screenPlay
	a.overlay = overlayNone
	a.paused, a.pauseWhy = false, ""
	a.countdown, a.acc, a.overAt = 0, 0, 0
	a.runBest = false
	a.birdAngle = -15
	a.exitReason = ""
	a.fx.list = a.fx.list[:0]
	a.setTheme(themeFor(0), a.themeTo.name == themes[0].name)
	a.dirty = true
}

func crashReason(c game.Crash) string {
	if c == game.CrashGround {
		return store.ReasonGround
	}
	return store.ReasonPipe
}

// plausible rejects scores the simulation could not have produced, which can
// only happen through a bug. It guards the leaderboard, not against players.
func (a *App) plausible(r store.Run) bool {
	p := a.opt.Params
	maxSpeed, minSpacing := 0.0, math.Inf(1)
	for _, s := range p.Stages {
		maxSpeed = math.Max(maxSpeed, s.Speed)
		minSpacing = math.Min(minSpacing, s.Spacing)
	}
	limit := float64(r.Ticks)*p.Dt()*maxSpeed/minSpacing + 2
	return float64(r.Score) <= limit
}

func (a *App) buildRun(reason string) store.Run {
	w := a.world
	return store.Run{
		UUID:     a.runID,
		Score:    w.Score(),
		Duration: time.Duration(w.Duration() * float64(time.Second)),
		Ticks:    w.Ticks(),
		Flaps:    w.Flaps(),
		Seed:     w.Seed(),
		Reason:   reason,
		EndedAt:  a.now(),
	}
}

func (a *App) finishRun() tea.Cmd {
	a.screen = screenOver
	a.overAt = a.t
	a.lastCrash = a.world.Crash()
	r := a.buildRun(crashReason(a.world.Crash()))
	a.lastRun = r
	a.newRecord = r.Score > 0 && r.Score > a.Best()
	a.sessionBest = max(a.sessionBest, r.Score)
	a.runs++
	a.dirty = true
	if !a.plausible(r) {
		a.log.Error("implausible score rejected", "score", r.Score, "ticks", r.Ticks)
		a.save, a.saveErr = saveSkipped, "score rejected"
		return nil
	}
	a.save, a.saveErr = saveBusy, ""
	return a.saveCmd(r, 1)
}

func (a *App) saveCmd(r store.Run, attempt int) tea.Cmd {
	st, resolve, pid, timeout := a.st, a.sess.Resolve, a.sess.Player.ID, a.opt.StoreTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var pl *store.Player
		if pid == 0 {
			if resolve == nil {
				return savedMsg{run: r, err: errNoStorage, attempt: attempt}
			}
			p, err := resolve(ctx)
			if err != nil {
				return savedMsg{run: r, err: err, attempt: attempt}
			}
			pid, pl = p.ID, &p
		}
		res, err := st.RecordRun(ctx, pid, r)
		return savedMsg{run: r, res: res, player: pl, err: err, attempt: attempt}
	}
}

func (a *App) onSaved(msg savedMsg) tea.Cmd {
	a.dirty = true
	if msg.player != nil && a.sess.Player.ID == 0 {
		a.sess.Player = *msg.player
		a.sess.StorageDown = false
	}
	current := msg.run.UUID == a.lastRun.UUID
	if msg.err != nil {
		a.log.Warn("saving run failed", "attempt", msg.attempt, "err", msg.err)
		if !current {
			return nil
		}
		if msg.attempt < 3 && !errors.Is(msg.err, store.ErrInvalidRun) {
			a.save = saveRetrying
			delay := time.Duration(msg.attempt) * 1500 * time.Millisecond
			return a.after(delay, retrySaveMsg{run: msg.run, attempt: msg.attempt + 1})
		}
		a.save, a.saveErr = saveFailed, "storage unavailable"
		return nil
	}
	a.best = max(a.best, msg.res.Best)
	if current {
		a.save = saveOK
		a.newRecord = msg.res.NewBest
		a.rank, a.ranked = msg.res.Rank, msg.res.Ranked
	}
	return a.loadTop()
}

// SaveUnsaved stores a run that was interrupted (quit, disconnect, shutdown)
// or whose save did not complete. It is called by the session after the
// program exits, so it runs synchronously.
func (a *App) SaveUnsaved(ctx context.Context, reason string) (saved bool, err error) {
	if a.exitReason != "" {
		reason = a.exitReason
	}
	var r store.Run
	switch {
	case a.screen == screenPlay && a.world != nil && a.world.Phase() != game.Over && a.world.Score() > 0:
		r = a.buildRun(reason)
	case a.screen == screenPlay && a.world != nil && a.world.Phase() == game.Over && a.save == saveNone:
		r = a.buildRun(crashReason(a.world.Crash()))
	case a.lastRun.UUID != "" && (a.save == saveBusy || a.save == saveRetrying || a.save == saveFailed):
		r = a.lastRun
	default:
		return false, nil
	}
	if !a.plausible(r) {
		return false, errors.New("implausible score rejected")
	}
	pid := a.sess.Player.ID
	if pid == 0 {
		if a.sess.Resolve == nil {
			return false, errNoStorage
		}
		p, err := a.sess.Resolve(ctx)
		if err != nil {
			return false, err
		}
		pid = p.ID
	}
	if _, err := a.st.RecordRun(ctx, pid, r); err != nil {
		return false, err
	}
	a.sessionBest = max(a.sessionBest, r.Score)
	return true, nil
}

func (a *App) loadTop() tea.Cmd {
	if a.topLoading || a.st == nil {
		return nil
	}
	a.topLoading = true
	st, pid, timeout := a.st, a.sess.Player.ID, a.opt.StoreTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		page, err := st.Leaderboard(ctx, 0, 5)
		var rank, ranked int
		if err == nil && pid != 0 {
			rank, ranked, err = st.Rank(ctx, pid)
		}
		return topMsg{page: page, rank: rank, ranked: ranked, err: err}
	}
}

func (a *App) boardRows() int {
	return max(3, a.h-a.lay.groundRows-9)
}

func (a *App) loadBoard() tea.Cmd {
	if a.boardLoading || a.st == nil {
		return nil
	}
	a.boardLoading = true
	rows := min(a.boardRows(), a.opt.LeaderboardSize)
	offset := a.boardPage * rows
	limit := min(rows, a.opt.LeaderboardSize-offset)
	st, pid, timeout := a.st, a.sess.Player.ID, a.opt.StoreTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		page, err := st.Leaderboard(ctx, offset, max(limit, 0))
		msg := boardMsg{offset: offset, page: page, err: err}
		if err == nil && pid != 0 {
			msg.rank, msg.ranked, _ = st.Rank(ctx, pid)
			msg.recent, _ = st.RecentRuns(ctx, pid, 8)
		}
		return msg
	}
}

func (a *App) onBoard(msg boardMsg) {
	a.boardLoading = false
	a.boardErr = msg.err != nil
	a.dirty = true
	if msg.err != nil {
		return
	}
	a.board = msg.page.Entries
	a.boardTotal = min(msg.page.Total, a.opt.LeaderboardSize)
	if msg.ranked > 0 {
		a.rank, a.ranked = msg.rank, msg.ranked
	}
	a.recent = msg.recent
}

func (a *App) boardPages() int {
	rows := min(a.boardRows(), a.opt.LeaderboardSize)
	return max(1, (a.boardTotal+rows-1)/rows)
}

func (a *App) openBoard() tea.Cmd {
	if a.screen != screenBoard {
		a.prevScreen = a.screen
	}
	a.screen = screenBoard
	a.overlay = overlayNone
	a.boardPage = 0
	return a.loadBoard()
}

// speedPercent is the current scroll speed relative to the starting speed.
func (a *App) speedPercent() int {
	if a.world == nil {
		return 100
	}
	return int(math.Round(100 * a.world.Speed() / a.opt.Params.StageAt(0).Speed))
}

// ringBell queues a terminal bell when sound is on. Bells less than 0.35 s
// apart are merged so events in quick succession do not stack up.
func (a *App) ringBell() {
	if a.sound != SoundOn || a.t-a.lastBell < 0.35 {
		return
	}
	a.lastBell = a.t
	a.soundOut.WriteString(bell)
}

// flushSound sends queued sound effects along with cmd.
func (a *App) flushSound(cmd tea.Cmd) tea.Cmd {
	if a.soundOut.Len() == 0 {
		return cmd
	}
	seq := a.soundOut.String()
	a.soundOut.Reset()
	return tea.Batch(cmd, tea.Raw(seq))
}

// toggleSound switches sound on or off and rings once when turned on so
// the player knows what to listen for.
func (a *App) toggleSound() tea.Cmd {
	if a.sound == SoundOn {
		a.sound = SoundOff
	} else {
		a.sound = SoundOn
		a.lastBell = -1
		a.ringBell()
	}
	return a.savePrefs()
}

func (a *App) savePrefs() tea.Cmd {
	if a.sess.Player.ID == 0 || a.st == nil {
		a.prefsDirty = false
		return nil
	}
	if a.prefsBusy {
		a.prefsDirty = true
		return nil
	}
	a.prefsBusy, a.prefsDirty = true, false
	st, id, timeout := a.st, a.sess.Player.ID, a.opt.StoreTimeout
	prefs := store.Prefs{ColorMode: a.color.String(), GlyphMode: a.glyphs.String(), SoundMode: a.sound.String()}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return prefsMsg{err: st.SetPrefs(ctx, id, prefs)}
	}
}

func (a *App) onRenamed(msg renamedMsg) {
	a.nameBusy = false
	a.dirty = true
	switch {
	case msg.err == nil:
		a.sess.Player.Name = msg.name
		a.overlay = overlayNone
	case errors.Is(msg.err, store.ErrNameTaken):
		a.nameErr = "That name is taken (or looks like one that is)."
	case errors.Is(msg.err, security.ErrNameLength), errors.Is(msg.err, security.ErrNameChars),
		errors.Is(msg.err, security.ErrNameStart), errors.Is(msg.err, security.ErrNameReserved):
		a.nameErr = msg.err.Error()
	default:
		a.log.Warn("rename failed", "err", msg.err)
		a.nameErr = "Couldn't save right now. Try again later."
	}
}

func (a *App) toggleGlyphs() {
	if a.glyphs == Unicode {
		a.glyphs = ASCII
	} else {
		a.glyphs = Unicode
	}
	a.g = glyphsFor(a.glyphs)
	a.scene.skyValid = false
	a.resize(a.w, a.h)
}

func normKey(msg tea.KeyPressMsg) string {
	k := msg.String()
	if len(k) == 1 && k[0] >= 'A' && k[0] <= 'Z' {
		k = strings.ToLower(k)
	}
	return k
}

func (a *App) key(msg tea.KeyPressMsg) tea.Cmd {
	k := normKey(msg)
	if k == "ctrl+c" {
		a.markQuit()
		return tea.Quit
	}
	if a.quitAt > 0 {
		return nil
	}
	a.dirty = true

	switch a.overlay {
	case overlayHelp:
		switch k {
		case "q":
			a.markQuit()
			return tea.Quit
		case "h", "?", "esc", "space", "enter", "backspace":
			a.overlay = overlayNone
		}
		return nil
	case overlayName:
		return a.keyName(msg, k)
	}

	switch a.screen {
	case screenWelcome:
		switch k {
		case "space", "enter":
			a.startRun()
		case "q", "esc":
			a.markQuit()
			return tea.Quit
		case "l":
			return a.openBoard()
		case "h", "?":
			a.overlay = overlayHelp
		case "n":
			if !a.sess.Guest && a.sess.Player.ID != 0 {
				a.overlay = overlayName
				a.nameInput = a.sess.Player.Name
				a.nameErr = ""
			}
		case "c":
			a.color = a.color.Next()
			return a.savePrefs()
		case "u":
			a.toggleGlyphs()
			return a.savePrefs()
		case "s":
			return a.toggleSound()
		}
	case screenPlay:
		return a.keyPlay(k)
	case screenOver:
		switch k {
		case "space", "enter":
			if a.t-a.overAt > 0.6 {
				a.startRun()
			}
		case "q", "esc":
			a.markQuit()
			return tea.Quit
		case "l":
			return a.openBoard()
		case "h", "?":
			a.overlay = overlayHelp
		case "c":
			a.color = a.color.Next()
			return a.savePrefs()
		case "u":
			a.toggleGlyphs()
			return a.savePrefs()
		case "s":
			return a.toggleSound()
		}
	case screenBoard:
		pages := a.boardPages()
		switch k {
		case "q":
			a.markQuit()
			return tea.Quit
		case "l", "esc", "b", "backspace", "space", "enter":
			a.screen = a.prevScreen
			if a.screen == screenBoard || a.screen == screenPlay {
				a.screen = screenWelcome
			}
		case "right", "pgdown", "down", "j", "n":
			if a.boardPage+1 < pages {
				a.boardPage++
				return a.loadBoard()
			}
		case "left", "pgup", "up", "k", "p":
			if a.boardPage > 0 {
				a.boardPage--
				return a.loadBoard()
			}
		case "home", "g":
			a.boardPage = 0
			return a.loadBoard()
		case "end":
			a.boardPage = pages - 1
			return a.loadBoard()
		}
	}
	return nil
}

func (a *App) keyPlay(k string) tea.Cmd {
	w := a.world
	switch k {
	case "space", "up":
		switch {
		case a.paused:
			a.resume()
		case a.countdown > 0 || a.lay.tooSmall:
		default:
			if w.Flap() {
				// The bird changes on the next frame; skip a redundant render.
				a.dirty = false
			}
		}
	case "p":
		if w.Phase() != game.Playing {
			return nil
		}
		if a.paused {
			a.resume()
		} else {
			a.pause("Paused")
		}
	case "h", "?":
		if w.Phase() == game.Playing && !a.paused {
			a.pause("Paused")
		}
		a.overlay = overlayHelp
	case "c":
		if a.paused {
			a.color = a.color.Next()
			return a.savePrefs()
		}
	case "u":
		if a.paused {
			a.toggleGlyphs()
			return a.savePrefs()
		}
	case "s":
		return a.toggleSound()
	case "q", "esc":
		a.markQuit()
		return tea.Quit
	}
	return nil
}

func (a *App) markQuit() {
	if a.exitReason == "" {
		a.exitReason = store.ReasonQuit
	}
}

func (a *App) keyName(msg tea.KeyPressMsg, k string) tea.Cmd {
	if a.nameBusy {
		return nil
	}
	switch k {
	case "esc":
		a.overlay = overlayNone
		return nil
	case "backspace":
		if n := len(a.nameInput); n > 0 {
			a.nameInput = a.nameInput[:n-1]
		}
		a.nameErr = ""
		return nil
	case "enter":
		name := a.nameInput
		if err := security.ValidateName(name); err != nil {
			a.nameErr = err.Error()
			return nil
		}
		if name == a.sess.Player.Name {
			a.overlay = overlayNone
			return nil
		}
		a.nameBusy = true
		st, id, timeout := a.st, a.sess.Player.ID, a.opt.StoreTimeout
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			return renamedMsg{name: name, err: st.Rename(ctx, id, name)}
		}
	}
	text := msg.Key().Text
	for _, r := range text {
		if security.NameAllowedRune(r) && len(a.nameInput) < security.MaxNameLen {
			a.nameInput += string(r)
			a.nameErr = ""
		}
	}
	return nil
}
