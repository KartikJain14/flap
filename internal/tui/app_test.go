package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"flap/internal/canvas"
	"flap/internal/game"
	"flap/internal/store"
)

var sgrRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// checkFrame verifies the encoded frame has exactly the terminal's size and
// carries no control characters besides SGR sequences and newlines.
func checkFrame(t *testing.T, h *harness, label string) {
	t.Helper()
	v := h.app.View()
	if !v.AltScreen {
		t.Fatalf("%s: view not in the alternate screen", label)
	}
	plain := sgrRe.ReplaceAllString(v.Content, "")
	for i, r := range plain {
		if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			t.Fatalf("%s: control character %U at %d", label, r, i)
		}
	}
	rows := strings.Split(plain, "\n")
	if len(rows) != h.app.h {
		t.Fatalf("%s: %d rows, want %d", label, len(rows), h.app.h)
	}
	for i, row := range rows {
		if w := canvas.StringWidth(row); w != h.app.w {
			t.Fatalf("%s: row %d has width %d, want %d: %q", label, i, w, h.app.w, row)
		}
	}
}

// visitAllScreens drives the app through every screen and overlay.
func visitAllScreens(t *testing.T, h *harness, label string) {
	t.Helper()
	h.frames(3, 12)
	checkFrame(t, h, label+"/welcome")
	h.key("h")
	checkFrame(t, h, label+"/help")
	h.key("h")
	h.key("n")
	checkFrame(t, h, label+"/name")
	h.key("esc")
	h.key("l")
	checkFrame(t, h, label+"/board")
	h.key("esc")
	h.key("space")
	h.autopilot(45)
	checkFrame(t, h, label+"/play")
	h.key("p")
	checkFrame(t, h, label+"/pause")
	h.key("space")
	h.frames(60, 30)
	for i := 0; i < 400 && h.app.screen == screenPlay; i++ {
		h.frames(1, 30)
	}
	h.frames(25, 30)
	checkFrame(t, h, label+"/over")
	h.key("l")
	checkFrame(t, h, label+"/board-from-over")
}

func TestEveryScreenFitsEverySize(t *testing.T) {
	sizes := [][2]int{
		{40, 12}, {50, 18}, {80, 24}, {100, 30}, {120, 40}, {160, 50},
		{1, 1}, {2, 60}, {49, 18}, {50, 17}, {51, 19}, {60, 50}, {220, 45}, {400, 200}, {300, 18}, {50, 120},
	}
	for _, sz := range sizes {
		for _, glyphs := range []GlyphMode{Unicode, ASCII} {
			label := fmt.Sprintf("%dx%d/%s", sz[0], sz[1], glyphs)
			h := newHarness(t, sz[0], sz[1], func(s *Session, o *Options) { s.Glyphs = glyphs })
			visitAllScreens(t, h, label)
		}
	}
}

func TestMinimumSizeIsPlayable(t *testing.T) {
	p := game.DefaultParams()
	l := computeLayout(50, 18, 50, 18, false)
	if l.tooSmall {
		t.Fatal("minimum size reported as too small")
	}
	minGap := p.Stages[len(p.Stages)-1].Gap
	if rows := minGap * l.unitY / float64(l.ppc); rows < 4 {
		t.Fatalf("tightest gap is only %.1f rows at the minimum size", rows)
	}
	if l.viewW < minViewWidth-1e-9 {
		t.Fatalf("view width %.2f below the minimum look-ahead", l.viewW)
	}
	sx, sy := birdScale(l, p)
	if sx*1.6 < 4.4 || sy*1.44 < 4 {
		t.Fatalf("bird sprite too small to read: %.1f cols x %.1f px", sx*1.6, sy*1.44)
	}
	// Look-ahead must show at least one full pipe spacing in front of the
	// bird at every supported size.
	for _, sz := range [][2]int{{50, 18}, {80, 24}, {100, 30}, {120, 40}, {160, 50}, {50, 60}, {400, 30}} {
		l := computeLayout(sz[0], sz[1], 50, 18, false)
		ahead := l.viewW - p.BirdX
		if ahead < p.Stages[0].Spacing {
			t.Errorf("%dx%d: only %.2f units visible ahead", sz[0], sz[1], ahead)
		}
		if l.viewW > maxViewWidth+1e-9 {
			t.Errorf("%dx%d: view width %.2f exceeds the maximum", sz[0], sz[1], l.viewW)
		}
	}
	if !computeLayout(40, 12, 50, 18, false).tooSmall {
		t.Fatal("40x12 should be too small")
	}
}

func TestSpaceStartsAndFlaps(t *testing.T) {
	h := newHarness(t, 80, 24)
	if h.app.screen != screenWelcome {
		t.Fatal("did not start on the welcome screen")
	}
	h.key("space")
	if h.app.screen != screenPlay || h.app.world == nil {
		t.Fatal("space did not start a run")
	}
	if h.app.world.Flaps() != 1 {
		t.Fatalf("run should start with a flap, got %d", h.app.world.Flaps())
	}
	h.frames(10, 30)
	vy := h.app.world.Bird().VY
	h.key("space")
	if h.app.world.Bird().VY >= vy || h.app.world.Flaps() != 2 {
		t.Fatal("space did not flap")
	}
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []string{"q", "esc", "ctrl+c"} {
		for _, where := range []string{"welcome", "play", "over"} {
			h := newHarness(t, 80, 24)
			switch where {
			case "play":
				h.key("space")
				h.frames(5, 30)
			case "over":
				h.key("space")
				for i := 0; i < 300 && h.app.screen == screenPlay; i++ {
					h.frames(1, 30)
				}
			}
			h.key(k)
			if !h.quit {
				t.Fatalf("%s on %s did not quit", k, where)
			}
			if h.app.ExitReason() != store.ReasonQuit {
				t.Fatalf("%s on %s: exit reason %q", k, where, h.app.ExitReason())
			}
		}
	}
	// ESC on the leaderboard goes back instead of quitting.
	h := newHarness(t, 80, 24)
	h.key("l")
	h.key("esc")
	if h.quit || h.app.screen != screenWelcome {
		t.Fatal("esc on the leaderboard should return to the welcome screen")
	}
	h.key("ctrl+c")
	if !h.quit {
		t.Fatal("ctrl+c must always quit")
	}
}

func TestResizeDuringPlayPausesAndPreservesState(t *testing.T) {
	h := newHarness(t, 100, 30)
	h.key("space")
	h.autopilot(30 * 4)
	w := h.app.world
	before := struct {
		score int
		bird  game.Bird
		cam   float64
		ticks int
	}{w.Score(), w.Bird(), w.Camera(), w.Ticks()}
	h.resize(80, 24)
	if !h.app.paused || h.app.pauseWhy != "Resized" {
		t.Fatal("resize during play did not pause")
	}
	h.frames(30, 30)
	if w.Ticks() != before.ticks || w.Bird() != before.bird || w.Camera() != before.cam || w.Score() != before.score {
		t.Fatal("world changed while paused after a resize")
	}
	if got := h.app.world.ViewWidth(); got != h.app.lay.viewW {
		t.Fatalf("view width not updated: %v vs %v", got, h.app.lay.viewW)
	}
	if !strings.Contains(h.screen(), "RESIZED") {
		t.Fatal("resize pause card not shown")
	}
	h.key("space")
	if h.app.paused || h.app.countdown <= 0 {
		t.Fatal("space should resume with a countdown")
	}
	h.frames(20, 30) // countdown is 1.5 s
	if w.Ticks() != before.ticks {
		t.Fatal("world advanced during the countdown")
	}
	h.frames(30, 30)
	if w.Ticks() == before.ticks {
		t.Fatal("world did not resume")
	}
}

func TestTooSmallTerminalPausesAndRecovers(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("space")
	h.autopilot(30 * 2)
	ticks := h.app.world.Ticks()
	for _, sz := range [][2]int{{40, 12}, {30, 8}, {49, 30}, {1, 1}} {
		h.resize(sz[0], sz[1])
		h.frames(10, 30)
		text := h.screen()
		if !strings.Contains(text, "too small") && sz[0] > 20 {
			t.Fatalf("%dx%d: resize notice missing:\n%s", sz[0], sz[1], text)
		}
		checkFrame(t, h, fmt.Sprintf("small %dx%d", sz[0], sz[1]))
	}
	if h.app.world.Ticks() != ticks {
		t.Fatal("simulation advanced while the terminal was too small")
	}
	h.resize(80, 24)
	if h.app.lay.tooSmall || !h.app.paused {
		t.Fatal("expected a paused run after restoring the size")
	}
	h.key("space")
	h.frames(60, 30)
	if h.app.world.Ticks() == ticks {
		t.Fatal("run did not continue after restoring the size")
	}
	// The welcome screen also recovers.
	w := newHarness(t, 30, 10)
	if !strings.Contains(w.screen(), "too small") {
		t.Fatal("welcome too-small notice missing")
	}
	w.resize(80, 24)
	if !strings.Contains(w.screen(), "PRESS") {
		t.Fatal("welcome screen not restored")
	}
}

func TestRepeatedResizeIsStable(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("space")
	for i := 0; i < 300; i++ {
		w, hh := 30+(i*37)%180, 8+(i*23)%60
		h.resize(w, hh)
		h.frames(1, 30)
		if i%3 == 0 {
			h.key("space")
		}
		checkFrame(t, h, fmt.Sprintf("resize %d (%dx%d)", i, w, hh))
	}
	if n := len(h.app.scene.sprites.m); n > 257 {
		t.Fatalf("sprite cache grew to %d entries", n)
	}
}

func TestGameOverSavesScoreAndShowsRank(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("space")
	h.autopilot(30 * 25)
	for i := 0; i < 30*30 && h.app.screen == screenPlay; i++ {
		h.frames(1, 30)
	}
	if h.app.screen != screenOver {
		t.Fatal("run never ended")
	}
	if h.app.save != saveOK {
		t.Fatalf("save state %v", h.app.save)
	}
	if len(h.st.runs) != 1 || h.st.runs[0].Score != h.app.lastRun.Score {
		t.Fatalf("stored runs %+v", h.st.runs)
	}
	r := h.st.runs[0]
	if r.Ticks == 0 || r.Seed != 42 || r.Reason == "" || len(r.UUID) != 32 {
		t.Fatalf("incomplete run record %+v", r)
	}
	text := h.screen()
	for _, want := range []string{"GAME OVER", "Score saved", "SCORE", "BEST"} {
		if !strings.Contains(text, want) {
			t.Fatalf("game over screen lacks %q:\n%s", want, text)
		}
	}
	// SPACE right away is ignored (cool-down), later it restarts.
	h.app.overAt = h.app.t
	h.key("space")
	if h.app.screen != screenOver {
		t.Fatal("restart was not debounced")
	}
	h.frames(30, 12)
	h.key("space")
	if h.app.screen != screenPlay {
		t.Fatal("space did not restart")
	}
}

func playUntilOver(h *harness, autopilotFrames int) {
	h.key("space")
	h.autopilot(autopilotFrames)
	for i := 0; i < 30*30 && h.app.screen == screenPlay; i++ {
		h.frames(1, 30)
	}
}

func TestSaveFailureRetriesThenRecovers(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.st.fail = 2 // two failures, then success on the third attempt
	playUntilOver(h, 30*6)
	if h.app.save != saveOK || len(h.st.runs) != 1 {
		t.Fatalf("expected success after retries, save=%v runs=%d", h.app.save, len(h.st.runs))
	}

	h.st.failAll = true
	h.frames(30, 12)
	h.key("space")
	h.autopilot(30 * 3)
	for i := 0; i < 30*30 && h.app.screen == screenPlay; i++ {
		h.frames(1, 30)
	}
	if h.app.save != saveFailed {
		t.Fatalf("save state %v, want failed", h.app.save)
	}
	if !strings.Contains(h.screen(), "Not saved") {
		t.Fatal("failure not reported to the player")
	}
	// Storage comes back: the failed run is saved when the session ends and
	// new runs save normally.
	h.st.failAll = false
	saved, err := h.app.SaveUnsaved(context.Background(), store.ReasonDisconnect)
	if err != nil || !saved || len(h.st.runs) != 2 {
		t.Fatalf("SaveUnsaved = %v, %v (runs %d)", saved, err, len(h.st.runs))
	}
}

func TestSaveUnsavedRunOnQuitAndDisconnect(t *testing.T) {
	for _, quit := range []bool{true, false} {
		h := newHarness(t, 80, 24)
		h.key("space")
		h.autopilot(30 * 8)
		if h.app.world.Score() < 1 {
			t.Fatal("autopilot did not score")
		}
		if quit {
			h.key("q")
		}
		saved, err := h.app.SaveUnsaved(context.Background(), store.ReasonDisconnect)
		if err != nil || !saved {
			t.Fatalf("quit=%v: SaveUnsaved = %v, %v", quit, saved, err)
		}
		want := store.ReasonDisconnect
		if quit {
			want = store.ReasonQuit
		}
		if got := h.st.runs[0].Reason; got != want {
			t.Fatalf("reason %q, want %q", got, want)
		}
		// Saving again records nothing new (idempotent).
		h.app.SaveUnsaved(context.Background(), store.ReasonDisconnect)
		if len(h.st.runs) != 1 {
			t.Fatalf("duplicate save: %d runs", len(h.st.runs))
		}
	}
	// Nothing to save on the welcome screen.
	h := newHarness(t, 80, 24)
	if saved, _ := h.app.SaveUnsaved(context.Background(), store.ReasonDisconnect); saved {
		t.Fatal("saved a run that never happened")
	}
}

func TestGuestProfileIsCreatedOnFirstSave(t *testing.T) {
	var created *store.Player
	var h *harness
	h = newHarness(t, 80, 24, func(s *Session, o *Options) {
		s.Guest = true
		s.Player = store.Player{Name: "guest-abcd", Kind: store.KindGuest}
		s.Resolve = func(ctx context.Context) (store.Player, error) {
			p := h.st.add("guest-abcd", 0, store.KindGuest)
			created = p
			return *p, nil
		}
	})
	playUntilOver(h, 30*4)
	if created == nil || h.app.sess.Player.ID != created.ID {
		t.Fatalf("guest profile not created/adopted: %+v", h.app.sess.Player)
	}
	if h.app.save != saveOK {
		t.Fatalf("save state %v", h.app.save)
	}
	// Guests cannot rename.
	h.frames(30, 12)
	h.key("esc")
	h.key("n")
	if h.app.overlay == overlayName {
		t.Fatal("guest opened the name editor")
	}
}

func TestNameEditingFiltersInput(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("n")
	if h.app.overlay != overlayName {
		t.Fatal("name editor not opened")
	}
	for range 20 {
		h.key("backspace")
	}
	for _, k := range []string{"S", "t", "a", "r", " ", "é", "\x1b", "-", "1", "_", "日"} {
		h.send(tea.KeyPressMsg{Code: []rune(k)[0], Text: k})
	}
	if h.app.nameInput != "Star-1_" {
		t.Fatalf("name input %q", h.app.nameInput)
	}
	h.key("enter")
	if h.app.overlay != overlayNone || h.app.sess.Player.Name != "Star-1_" || h.st.renames != 1 {
		t.Fatalf("rename failed: overlay=%v name=%q", h.app.overlay, h.app.sess.Player.Name)
	}
	// Taken names are refused with a message.
	h.key("n")
	for range 20 {
		h.key("backspace")
	}
	for _, k := range "Nimbus" {
		h.send(tea.KeyPressMsg{Code: k, Text: string(k)})
	}
	h.key("enter")
	if h.app.overlay != overlayName || !strings.Contains(h.app.nameErr, "taken") {
		t.Fatalf("taken name: overlay=%v err=%q", h.app.overlay, h.app.nameErr)
	}
	// Invalid names never reach the store.
	for range 20 {
		h.key("backspace")
	}
	h.key("x")
	h.key("enter")
	if h.st.renames != 1 || h.app.nameErr == "" {
		t.Fatal("short name was accepted")
	}
}

func TestHostileStoredNamesRenderSafely(t *testing.T) {
	h := newHarness(t, 100, 30)
	for _, n := range []string{"\x1b[2J\x1b]0;pwn\x07", "a\u202eb", "wide日本語名前がとても長い", "\u200b\u200b", "x\r\ny"} {
		h.st.add(n, 500, store.KindKey)
	}
	h.run(h.app.loadTop())
	h.key("l")
	v := h.app.View()
	if strings.Contains(sgrRe.ReplaceAllString(v.Content, ""), "\x1b") {
		t.Fatal("raw escape sequence from a stored name reached the terminal")
	}
	checkFrame(t, h, "hostile board")
}

func TestASCIIModeEmitsOnlyASCII(t *testing.T) {
	h := newHarness(t, 100, 30, func(s *Session, o *Options) { s.Glyphs = ASCII })
	check := func(label string) {
		plain := sgrRe.ReplaceAllString(h.app.View().Content, "")
		for _, r := range plain {
			if r > 0x7e {
				t.Fatalf("%s: non-ASCII rune %q in ASCII mode", label, r)
			}
		}
	}
	check("welcome")
	h.key("h")
	check("help")
	h.key("h")
	h.key("l")
	check("board")
	h.key("l")
	h.key("space")
	h.autopilot(60)
	check("play")
	h.key("p")
	check("pause")
}

func TestColorModesEncodeCorrectly(t *testing.T) {
	h := newHarness(t, 80, 24)
	modes := map[canvas.ColorMode][]string{
		canvas.TrueColor: {"38;2;", "48;2;"},
		canvas.Color256:  {"38;5;", "48;5;"},
	}
	for mode, want := range modes {
		h.app.color = mode
		h.app.dirty = true
		out := h.app.View().Content
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Fatalf("%v: missing %q", mode, w)
			}
		}
	}
	h.app.color = canvas.Color16
	h.app.dirty = true
	out := h.app.View().Content
	if strings.Contains(out, "38;5;") || strings.Contains(out, "38;2;") || strings.Contains(out, "48;2;") {
		t.Fatal("16-colour mode emitted extended colours")
	}
	// C cycles the mode and persists it.
	h.app.color = canvas.TrueColor
	h.key("c")
	if h.app.color != canvas.Color256 || h.st.prefs.ColorMode != "256" {
		t.Fatalf("colour toggle: %v, stored %q", h.app.color, h.st.prefs.ColorMode)
	}
	h.key("u")
	if h.app.glyphs != ASCII || h.st.prefs.GlyphMode != "ascii" {
		t.Fatal("glyph toggle not applied/persisted")
	}
}

func TestPauseFreezesSimulation(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("space")
	h.frames(10, 30)
	h.key("p")
	ticks := h.app.world.Ticks()
	h.frames(90, 30)
	if h.app.world.Ticks() != ticks {
		t.Fatal("paused world changed")
	}
	// SPACE while paused resumes (after a countdown) instead of flapping.
	h.key("space")
	if h.app.world.Flaps() != 1 || h.app.paused {
		t.Fatal("space while paused should resume without flapping")
	}
	h.frames(90, 30)
	if h.app.world.Ticks() == ticks {
		t.Fatal("world did not resume")
	}
}

func TestServerNoticeQuits(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("space")
	h.frames(10, 30)
	h.send(NoticeMsg{Text: "Server restarting", Quit: true, Reason: store.ReasonShutdown})
	if !strings.Contains(h.screen(), "Server restarting") {
		t.Fatal("notice not shown")
	}
	h.frames(30, 12)
	if !h.quit {
		t.Fatal("notice did not quit")
	}
	if h.app.ExitReason() != store.ReasonShutdown {
		t.Fatalf("exit reason %q", h.app.ExitReason())
	}
}

func TestFlapSkipsRedundantRender(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("space")
	h.frames(2, 30)
	h.app.View()
	h.key("space")
	if h.app.dirty {
		t.Fatal("a flap should not force a re-render before the next tick")
	}
	h.frames(1, 30)
	if !h.app.dirty {
		t.Fatal("tick must mark the frame dirty")
	}
}

func TestLaggyTicksDoNotTeleport(t *testing.T) {
	h := newHarness(t, 80, 24)
	h.key("space")
	h.frames(5, 30)
	ticks := h.app.world.Ticks()
	// A 3 second stall on the server is clamped: the world advances at most
	// 12 steps (0.2 s) instead of 180.
	h.now = h.now.Add(3 * time.Second)
	h.send(TickMsg{At: h.now})
	if d := h.app.world.Ticks() - ticks; d > 12 {
		t.Fatalf("world advanced %d steps after a stall", d)
	}
}

func BenchmarkFrame(b *testing.B) {
	for _, sz := range [][2]int{{50, 18}, {80, 24}, {100, 30}, {120, 40}, {160, 50}} {
		b.Run(fmt.Sprintf("%dx%d", sz[0], sz[1]), func(b *testing.B) {
			t := &testing.T{}
			h := newHarness(t, sz[0], sz[1])
			h.key("space")
			h.autopilot(60)
			var bytesOut int
			b.ReportAllocs()
			for b.Loop() {
				h.frames(1, 30)
				if wantsFlap(h.app.world) {
					h.key("space")
				}
				if h.app.screen != screenPlay {
					h.key("space")
				}
				bytesOut = len(h.app.View().Content)
			}
			b.ReportMetric(float64(bytesOut), "bytes/view")
		})
	}
}

func bells(h *harness) int {
	n := 0
	for _, r := range h.raw {
		n += strings.Count(r, bell)
	}
	return n
}

func crash(h *harness) {
	for i := 0; i < 30*6 && h.app.screen == screenPlay; i++ {
		h.frames(1, 30)
	}
}

func TestSoundEffects(t *testing.T) {
	h := newHarness(t, 80, 24)
	if h.app.sound != SoundOn {
		t.Fatal("sound should default to on")
	}
	h.key("space")
	crash(h)
	if got := bells(h); got != 1 {
		t.Fatalf("crash rang %d bells, want 1", got)
	}
	for _, r := range h.raw {
		if strings.Trim(r, bell) != "" {
			t.Fatalf("raw output other than BEL: %q", r)
		}
	}
	// S turns sound off, persists it, and stays silent.
	h.frames(30, 12)
	h.key("s")
	if h.app.sound != SoundOff || h.st.prefs.SoundMode != "off" {
		t.Fatalf("sound %v, stored %q", h.app.sound, h.st.prefs.SoundMode)
	}
	before := bells(h)
	h.key("space")
	crash(h)
	if bells(h) != before {
		t.Fatal("bell rang while sound was off")
	}
	// Turning it back on rings once so the player knows what to expect.
	h.key("s")
	if h.app.sound != SoundOn || h.st.prefs.SoundMode != "on" || bells(h) != before+1 {
		t.Fatalf("sound on: %v, stored %q, bells %d", h.app.sound, h.st.prefs.SoundMode, bells(h)-before)
	}
	w := newHarness(t, 110, 30)
	w.key("s")
	if !strings.Contains(w.screen(), "S sound: off") {
		t.Fatalf("welcome status bar does not show the sound setting:\n%s", w.screen())
	}
}

func TestStoredSoundPreferenceIsRespected(t *testing.T) {
	h := newHarness(t, 80, 24, func(s *Session, o *Options) { s.Sound = SoundOff })
	h.key("space")
	crash(h)
	if bells(h) != 0 {
		t.Fatal("bell rang although the player turned sound off")
	}
}

func TestSpeedUpRingsAndShowsSpeed(t *testing.T) {
	h := newHarness(t, 100, 30)
	h.key("space")
	if !strings.Contains(h.screen(), "SPEED 100%") {
		t.Fatalf("HUD lacks the starting speed:\n%s", h.screen())
	}
	for i := 0; i < 30*120 && h.app.world.Score() < 10; i++ {
		h.autopilot(1)
		if h.app.screen != screenPlay {
			t.Fatalf("autopilot crashed at %d", h.app.world.Score())
		}
	}
	h.autopilot(3)
	if h.app.world.Stage() != 1 || bells(h) != 1 {
		t.Fatalf("stage %d, bells %d after reaching 10 points", h.app.world.Stage(), bells(h))
	}
	text := h.screen()
	pct := h.app.speedPercent()
	if pct <= 100 || !strings.Contains(text, fmt.Sprintf("SPEED %d%%", pct)) || !strings.Contains(text, "MIDDAY") {
		t.Fatalf("speed-up not shown (speed %d%%):\n%s", pct, text)
	}
	// Speed keeps creeping up with every point, not only at stage changes.
	for i := 0; i < 30*30 && h.app.world.Score() < 14; i++ {
		h.autopilot(1)
	}
	if h.app.speedPercent() <= pct {
		t.Fatalf("speed did not increase between stages: %d%% -> %d%%", pct, h.app.speedPercent())
	}
}
