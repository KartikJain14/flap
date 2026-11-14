package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"flap/internal/game"
	"flap/internal/store"
)

func wingHarness(t *testing.T, w, h int) *harness {
	hs := newHarness(t, w, h, func(s *Session, o *Options) { s.Player.Wings = 3 })
	hs.st.players[hs.app.sess.Player.ID].Wings = 3
	return hs
}

func crashForWings(h *harness) {
	if h.app.world == nil {
		h.key("space")
	}
	for i := 0; i < 300 && !h.app.continueOffer; i++ {
		h.frames(1, 30)
	}
	if !h.app.continueOffer {
		h.t.Fatal("no continue offer")
	}
}

func TestContinueChoiceAndDoubling(t *testing.T) {
	h := wingHarness(t, 80, 24)
	h.key("space")
	h.autopilot(120)
	crashForWings(h)
	w := h.app.world
	score, cam, id := w.Score(), w.Camera(), h.app.runID
	if len(h.st.runs) != 0 {
		t.Fatal("saved before continue choice")
	}
	if !strings.Contains(h.screen(), "CONTINUE COST 1") {
		t.Fatal(h.screen())
	}
	h.key("w")
	if h.app.continueOffer || w.Phase() != game.Playing || h.app.sess.Player.Wings != 2 {
		t.Fatal("continue failed")
	}
	if w.Score() != score || w.Camera() != cam || h.app.runID != id {
		t.Fatal("continue reset the run")
	}
	h.key("w")
	if h.app.sess.Player.Wings != 2 {
		t.Fatal("duplicate key charged twice")
	}
	h.frames(15, 30)
	if w.Invincible() != 3 {
		t.Fatal("countdown used shield time")
	}
	crashForWings(h)
	if !strings.Contains(h.screen(), "CONTINUE COST 2") {
		t.Fatal(h.screen())
	}
	h.key("w")
	if h.app.sess.Player.Wings != 0 || h.app.retries != 2 {
		t.Fatal("second continue cost")
	}
	for i := 0; i < 400 && h.app.screen == screenPlay; i++ {
		h.frames(1, 30)
	}
	if h.app.screen != screenOver || len(h.st.runs) != 1 {
		t.Fatal("continued run was not saved exactly once")
	}
	h.frames(30, 30)
	h.key("space")
	if h.app.retries != 0 {
		t.Fatal("new run did not reset cost")
	}
}

func TestContinueFailureAndDisconnect(t *testing.T) {
	h := wingHarness(t, 50, 18)
	crashForWings(h)
	h.st.fail = 1
	h.key("w")
	if !h.app.continueOffer || h.app.world.Phase() != game.Over || h.app.sess.Player.Wings != 3 {
		t.Fatal("failed purchase revived or charged")
	}
	if !strings.Contains(h.screen(), "Couldn't use wings") {
		t.Fatal(h.screen())
	}
	saved, err := h.app.SaveUnsaved(context.Background(), store.ReasonDisconnect)
	if err != nil || !saved || len(h.st.runs) != 1 {
		t.Fatalf("pending disconnect: %v %v", saved, err)
	}
	h.key("enter")
	if h.app.screen != screenOver || len(h.st.runs) != 1 {
		t.Fatal("declining saved twice")
	}
}

func TestContinueWaitsForPurchaseAndResize(t *testing.T) {
	h := wingHarness(t, 80, 24)
	crashForWings(h)
	cmd := h.app.keyPlay("w") // leave command in flight
	h.key("w")
	h.key("space")
	h.key("enter")
	if !h.app.wingsBusy || h.app.screen != screenPlay || !h.app.continueOffer {
		t.Fatal("in-flight purchase was dismissed")
	}
	h.run(cmd)
	h.frames(60, 30)
	shield := h.app.world.Invincible()
	h.resize(40, 12)
	h.frames(90, 30)
	if h.app.world.Invincible() != shield {
		t.Fatal("resize consumed protection")
	}
	h.resize(80, 24)
	h.key("space")
	h.frames(60, 30)
	if h.app.world.Invincible() >= shield {
		t.Fatal("protection did not resume")
	}
}

func TestGuestCanSpendStarterWings(t *testing.T) {
	var h *harness
	h = newHarness(t, 80, 24, func(s *Session, o *Options) {
		s.Guest = true
		s.Player = store.Player{Name: "guest-wing", Kind: store.KindGuest}
		s.Resolve = func(context.Context) (store.Player, error) {
			p := h.st.add("guest-wing", 0, store.KindGuest)
			p.Wings = 3
			return *p, nil
		}
	})
	crashForWings(h)
	if h.app.sess.Player.ID != 0 {
		t.Fatal("guest allocated before purchase")
	}
	h.key("w")
	if h.app.sess.Player.ID == 0 || h.app.sess.Player.Wings != 2 || h.app.world.Phase() != game.Playing {
		t.Fatal("guest could not continue")
	}
}

func TestContinueInsufficientBalanceAndStaleReply(t *testing.T) {
	h := wingHarness(t, 80, 24)
	crashForWings(h)
	h.st.players[h.app.sess.Player.ID].Wings = 0 // another session spent it
	h.key("w")
	if !h.app.continueOffer || h.app.world.Phase() != game.Over || h.app.sess.Player.Wings != 0 {
		t.Fatal("overspent stale balance")
	}
	h.send(wingsMsg{runID: "old-run", balance: 100})
	if h.app.sess.Player.Wings != 0 {
		t.Fatal("accepted stale reply")
	}
	h.key("enter")
	if h.app.screen != screenOver {
		t.Fatal("could not decline")
	}
}

func TestWingScreensFit(t *testing.T) {
	for _, sz := range [][2]int{{50, 18}, {80, 24}, {100, 30}, {160, 50}} {
		for _, glyph := range []GlyphMode{Unicode, ASCII} {
			h := wingHarness(t, sz[0], sz[1])
			h.app.glyphs = glyph
			h.app.g = glyphsFor(glyph)
			h.resize(sz[0], sz[1])
			crashForWings(h)
			s := h.screen()
			for _, want := range []string{"SPREAD YOUR WINGS", "WINGS 3", "CONTINUE COST 1", "continue", "new game"} {
				if !strings.Contains(s, want) {
					t.Fatalf("%v %v missing %q: %s", sz, glyph, want, s)
				}
			}
			h.key("w")
			h.frames(50, 30)
			if !strings.Contains(h.screen(), "WINGS SHIELD") {
				t.Fatal(h.screen())
			}
			h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		}
	}
}

func TestSpaceFinishesAndStartsFreshWithoutWings(t *testing.T) {
	h := wingHarness(t, 80, 24)
	h.key("space")
	h.autopilot(120)
	crashForWings(h)
	oldID, oldScore := h.app.runID, h.app.world.Score()
	h.key("space")
	if h.app.screen != screenPlay || h.app.continueOffer || h.app.world.Phase() != game.Playing || h.app.runID == oldID || h.app.world.Score() != 0 {
		t.Fatal("SPACE did not start a fresh flight")
	}
	if h.app.sess.Player.Wings != 3 || h.app.retries != 0 {
		t.Fatal("restart spent wings or retained retry cost")
	}
	if len(h.st.runs) != 1 || h.st.runs[0].UUID != oldID || h.st.runs[0].Score != oldScore {
		t.Fatal("previous flight was not saved exactly once")
	}
	h.key("space")
	if len(h.st.runs) != 1 || h.app.sess.Player.Wings != 3 {
		t.Fatal("flap saved or spent wings")
	}
}

func TestRestartPreservesPendingSaveOnImmediateQuit(t *testing.T) {
	h := wingHarness(t, 80, 24)
	crashForWings(h)
	oldID := h.app.runID
	cmd := h.app.keyPlay("space") // leave save command in flight
	if cmd == nil || h.app.save != saveBusy {
		t.Fatal("restart lost pending save")
	}
	saved, err := h.app.SaveUnsaved(context.Background(), store.ReasonDisconnect)
	if err != nil || !saved || len(h.st.runs) != 1 || h.st.runs[0].UUID != oldID {
		t.Fatalf("lost crashed flight on disconnect: %v %v", saved, err)
	}
	h.run(cmd)
	if len(h.st.runs) != 1 {
		t.Fatal("late command duplicated previous flight")
	}
	// Crashing a new flight before its game-over transition must still save
	// that flight, regardless of the previous flight's successful save state.
	for h.app.world.Phase() != game.Over {
		h.app.world.Step()
	}
	saved, err = h.app.SaveUnsaved(context.Background(), store.ReasonDisconnect)
	if err != nil || !saved || len(h.st.runs) != 2 {
		t.Fatalf("lost new flight: %v %v", saved, err)
	}
}

func TestContinueCountdownExpires(t *testing.T) {
	h := wingHarness(t, 80, 24)
	crashForWings(h)
	if !strings.Contains(h.screen(), "CONTINUE? 5") {
		t.Fatal(h.screen())
	}
	h.frames(60, 30)
	if !h.app.continueOffer || !strings.Contains(h.screen(), "CONTINUE? 4") && !strings.Contains(h.screen(), "CONTINUE? 3") {
		t.Fatal(h.screen())
	}
	h.frames(100, 30)
	if h.app.screen != screenOver || len(h.st.runs) != 1 || h.app.sess.Player.Wings != 3 {
		t.Fatal("timeout failed to finish without spending")
	}
	h.frames(90, 30)
	if len(h.st.runs) != 1 {
		t.Fatal("timeout saved twice")
	}
}

func TestContinueCountdownWaitsForPurchase(t *testing.T) {
	h := wingHarness(t, 80, 24)
	crashForWings(h)
	cmd := h.app.keyPlay("w")
	left := h.app.continueLeft
	h.frames(180, 30)
	if !h.app.continueOffer || h.app.continueLeft != left {
		t.Fatal("expired during purchase")
	}
	h.run(cmd)
	if h.app.world.Phase() != game.Playing {
		t.Fatal("purchase did not revive")
	}
}

func TestWingPickupCreditsImmediately(t *testing.T) {
	h := wingHarness(t, 80, 24)
	h.key("space")
	// Follow the centre of the gap for the collectible pipe. Other pipes use
	// the normal pilot. This exercises actual simulation contact and commands.
	for i := 0; i < 30*100 && len(h.app.world.CollectedWings()) == 0; i++ {
		w := h.app.world
		if w.Phase() != game.Playing {
			t.Fatal("pilot crashed before pickup")
		}
		aim := 0.0
		for _, p := range w.Pipes() {
			if p.X+w.Params().PipeWidth+w.Params().BirdWidth/2 > w.BirdWorldX() {
				if p.HasWing() {
					aim = p.GapY
				}
				break
			}
		}
		b := w.Bird()
		if aim != 0 {
			if b.Y > aim-0.025 && b.VY > -w.Params().FlapVelocity*0.25 {
				h.key("space")
			}
		} else if wantsFlap(w) {
			h.key("space")
		}
		h.frames(1, 30)
	}
	if len(h.app.world.CollectedWings()) != 1 || h.app.sess.Player.Wings != 4 || h.st.players[h.app.sess.Player.ID].Wings != 4 {
		t.Fatal("pickup was not immediately credited")
	}
	if len(h.st.runs) != 0 {
		t.Fatal("pickup prematurely ended flight")
	}
	if !strings.Contains(h.app.banner, "WING COLLECTED") {
		t.Fatal("missing pickup animation banner")
	}
	h.run(h.app.collectCmd())
	if h.app.sess.Player.Wings != 4 {
		t.Fatal("repeated sync duplicated reward")
	}
	crashForWings(h)
	h.key("w")
	if h.app.sess.Player.Wings != 3 || h.app.world.Phase() != game.Playing {
		t.Fatal("could not spend collected wing in same run")
	}
}
