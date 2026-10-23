package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"flap/internal/canvas"
	"flap/internal/game"
	"flap/internal/store"
)

// fakeStore is an in-memory Store with injectable failures.
type fakeStore struct {
	mu       sync.Mutex
	players  map[int64]*store.Player
	runs     []store.Run
	fail     int // number of upcoming calls that fail
	failAll  bool
	renames  int
	prefs    store.Prefs
	nextID   int64
	recorded map[string]bool
}

var errFake = errors.New("fake storage failure")

func newFakeStore() *fakeStore {
	return &fakeStore{players: map[int64]*store.Player{}, recorded: map[string]bool{}, nextID: 1}
}

func (f *fakeStore) failing() bool {
	if f.failAll {
		return true
	}
	if f.fail > 0 {
		f.fail--
		return true
	}
	return false
}

func (f *fakeStore) add(name string, best int, kind store.Kind) *store.Player {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &store.Player{ID: f.nextID, Name: name, Best: best, Kind: kind, BestAt: time.Unix(1700000000, 0)}
	f.nextID++
	f.players[p.ID] = p
	return p
}

func (f *fakeStore) RecordRun(_ context.Context, id int64, r store.Run) (store.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing() {
		return store.RunResult{}, errFake
	}
	p, ok := f.players[id]
	if !ok {
		return store.RunResult{}, store.ErrNotFound
	}
	res := store.RunResult{PlayerID: id}
	if f.recorded[r.UUID] {
		res.Duplicate = true
	} else {
		f.recorded[r.UUID] = true
		f.runs = append(f.runs, r)
		p.Runs++
		if r.Score > p.Best {
			p.Best = r.Score
			res.NewBest = true
		}
	}
	res.Best = p.Best
	res.Rank, res.Ranked = f.rankLocked(id)
	return res, nil
}

func (f *fakeStore) sorted() []*store.Player {
	var ps []*store.Player
	for _, p := range f.players {
		if p.Best > 0 {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Best != ps[j].Best {
			return ps[i].Best > ps[j].Best
		}
		return ps[i].ID < ps[j].ID
	})
	return ps
}

func (f *fakeStore) rankLocked(id int64) (int, int) {
	ps := f.sorted()
	for i, p := range ps {
		if p.ID == id {
			return i + 1, len(ps)
		}
	}
	return 0, len(ps)
}

func (f *fakeStore) Leaderboard(_ context.Context, offset, limit int) (store.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing() {
		return store.Page{}, errFake
	}
	ps := f.sorted()
	page := store.Page{Total: len(ps)}
	for i := offset; i < len(ps) && i < offset+limit; i++ {
		p := ps[i]
		page.Entries = append(page.Entries, store.Entry{Rank: i + 1, PlayerID: p.ID, Name: p.Name, Kind: p.Kind, Score: p.Best, At: p.BestAt, Runs: p.Runs})
	}
	return page, nil
}

func (f *fakeStore) Rank(_ context.Context, id int64) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing() {
		return 0, 0, errFake
	}
	r, t := f.rankLocked(id)
	return r, t, nil
}

func (f *fakeStore) RecentRuns(_ context.Context, id int64, limit int) ([]store.RunSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.RunSummary
	for i := len(f.runs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, store.RunSummary{Score: f.runs[i].Score, Reason: f.runs[i].Reason, EndedAt: f.runs[i].EndedAt})
	}
	return out, nil
}

func (f *fakeStore) Rename(_ context.Context, id int64, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing() {
		return errFake
	}
	for _, p := range f.players {
		if p.ID != id && p.Name == name {
			return store.ErrNameTaken
		}
	}
	f.players[id].Name = name
	f.renames++
	return nil
}

func (f *fakeStore) SetPrefs(_ context.Context, id int64, p store.Prefs) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefs = p
	return nil
}

// harness drives an App synchronously: commands are executed inline (with
// timers skipped), as Bubble Tea would eventually deliver their results.
type harness struct {
	t     *testing.T
	app   *App
	st    *fakeStore
	now   time.Time
	quit  bool
	msgs  int
	raw   []string // raw terminal output requested by the model (sound)
	seed  uint64
	param game.Params
}

func newHarness(t *testing.T, w, h int, mutate ...func(*Session, *Options)) *harness {
	t.Helper()
	st := newFakeStore()
	st.add("SkyWalker", 112, store.KindKey)
	st.add("Nimbus", 98, store.KindKey)
	st.add("guest-7xq2", 61, store.KindGuest)
	me := st.add("BraveFinch42", 17, store.KindKey)
	st.add("Zephyr", 9, store.KindKey)
	hs := &harness{t: t, st: st, now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), seed: 42}
	sess := Session{Player: *me, KeyFingerprint: "SHA256:9f3aQx1Lw0vR", Color: canvas.TrueColor, Glyphs: Unicode, Width: w, Height: h}
	opt := Options{
		Params:    game.DefaultParams(),
		RenderFPS: 30, MenuFPS: 12, MinWidth: 50, MinHeight: 18,
		LeaderboardSize: 100, StoreTimeout: time.Second,
		ServerName: "flap.example.com",
		Online:     func() int { return 7 },
		Seed:       func() uint64 { return hs.seed },
		Now:        func() time.Time { return hs.now },
		NoTicks:    true,
	}
	for _, m := range mutate {
		m(&sess, &opt)
	}
	hs.param = opt.Params
	hs.app = New(st, sess, opt)
	hs.run(hs.app.Init())
	return hs
}

// run executes a command tree synchronously and feeds results back.
func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil || h.quit {
		return
	}
	msg := cmd()
	switch m := msg.(type) {
	case nil:
	case tea.QuitMsg:
		h.quit = true
	case tea.RawMsg:
		h.raw = append(h.raw, fmt.Sprint(m.Msg))
	case tea.BatchMsg:
		for _, c := range m {
			h.run(c)
		}
	default:
		h.send(m)
	}
}

func (h *harness) send(msg tea.Msg) {
	h.msgs++
	if h.msgs > 100000 {
		h.t.Fatal("message loop runaway")
	}
	_, cmd := h.app.Update(msg)
	h.run(cmd)
}

func (h *harness) key(k string) {
	var msg tea.KeyPressMsg
	switch k {
	case "space":
		msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "backspace":
		msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "right":
		msg = tea.KeyPressMsg{Code: tea.KeyRight}
	case "left":
		msg = tea.KeyPressMsg{Code: tea.KeyLeft}
	case "ctrl+c":
		msg = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	default:
		r := []rune(k)[0]
		msg = tea.KeyPressMsg{Code: r, Text: k}
	}
	h.send(msg)
}

// frames advances time by n frames at the given FPS.
func (h *harness) frames(n, fps int) {
	for i := 0; i < n && !h.quit; i++ {
		h.now = h.now.Add(time.Second / time.Duration(fps))
		h.send(TickMsg{At: h.now})
	}
}

func (h *harness) resize(w, hh int) { h.send(tea.WindowSizeMsg{Width: w, Height: hh}) }

// screen renders and returns the plain text of the current frame.
func (h *harness) screen() string {
	h.app.View()
	return h.app.cv.PlainText()
}

// autopilot plays well using the same strategy as the game tests.
func (h *harness) autopilot(frames int) {
	for i := 0; i < frames && !h.quit; i++ {
		w := h.app.world
		if h.app.screen == screenPlay && w != nil && w.Phase() == game.Playing && !h.app.paused && h.app.countdown == 0 {
			if wantsFlap(w) {
				h.key("space")
			}
		}
		h.frames(1, 30)
	}
}

func wantsFlap(w *game.World) bool {
	p := w.Params()
	b := w.Bird()
	bx := w.BirdWorldX()
	aim := 0.5
	for _, pp := range w.Pipes() {
		if pp.X+p.PipeWidth+p.BirdWidth/2 > bx {
			aim = pp.GapBottom() - p.BirdHeight/2 - pp.Gap*0.18
			break
		}
	}
	return b.Y > aim && b.VY > -p.FlapVelocity*0.25
}
