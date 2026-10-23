package tui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"flap/internal/canvas"
)

var shotsDir = flag.String("shots", "", "write SVG screenshots of every screen to this directory")

// TestShots renders representative frames for visual review and for the
// README. It only runs when -shots is given.
func TestShots(t *testing.T) {
	if *shotsDir == "" {
		t.Skip("pass -shots=DIR to render screenshots")
	}
	if err := os.MkdirAll(*shotsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	save := func(h *harness, name string, mode canvas.ColorMode) {
		h.app.View()
		f, err := os.Create(filepath.Join(*shotsDir, name+".svg"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := h.app.cv.WriteSVG(f, mode); err != nil {
			t.Fatal(err)
		}
	}
	sizes := [][2]int{{50, 18}, {80, 24}, {100, 30}, {120, 40}, {160, 50}}
	for _, sz := range sizes {
		w, hh := sz[0], sz[1]
		tag := fmt.Sprintf("%dx%d", w, hh)

		h := newHarness(t, w, hh)
		h.frames(20, 12)
		save(h, "welcome-"+tag, canvas.TrueColor)

		h.key("space")
		h.autopilot(30 * 9)
		save(h, "play-"+tag, canvas.TrueColor)

		// Crash on purpose and show the game over card.
		for i := 0; i < 30*4 && h.app.screen == screenPlay; i++ {
			h.frames(1, 30)
		}
		h.frames(10, 30)
		save(h, "over-"+tag, canvas.TrueColor)

		h.key("l")
		save(h, "board-"+tag, canvas.TrueColor)
	}

	// Later stages: actually play there so the HUD, theme and difficulty
	// match.
	h := newHarness(t, 100, 30)
	h.key("space")
	for stage := 0; stage < len(themes); stage++ {
		for i := 0; i < 30*600 && h.app.world.Stage() < stage; i++ {
			h.autopilot(1)
			if h.app.screen != screenPlay {
				t.Fatalf("autopilot crashed before stage %d (score %d)", stage, h.app.world.Score())
			}
		}
		h.autopilot(30 * 3) // let the sky transition finish
		save(h, fmt.Sprintf("stage%d-100x30", stage), canvas.TrueColor)
	}
	h = newHarness(t, 100, 30)
	h.key("space")
	h.autopilot(30 * 3)
	save(h, "play-100x30-256", canvas.Color256)
	save(h, "play-100x30-16", canvas.Color16)
	h.key("p")
	save(h, "pause-100x30", canvas.TrueColor)
	h.key("h")
	save(h, "help-100x30", canvas.TrueColor)

	a := newHarness(t, 100, 30, func(s *Session, o *Options) { s.Glyphs = ASCII })
	a.frames(5, 12)
	save(a, "welcome-100x30-ascii", canvas.TrueColor)
	a.key("space")
	a.autopilot(30 * 6)
	save(a, "play-100x30-ascii", canvas.TrueColor)

	small := newHarness(t, 40, 12)
	save(small, "toosmall-40x12", canvas.TrueColor)

	g := newHarness(t, 80, 24, func(s *Session, o *Options) {
		s.Guest = true
		s.Player.ID = 0
		s.Player.Name = "guest-k3m9"
		s.Player.Best = 0
	})
	save(g, "welcome-80x24-guest", canvas.TrueColor)

	n := newHarness(t, 80, 24)
	n.key("n")
	save(n, "name-80x24", canvas.TrueColor)

	wide := newHarness(t, 220, 45)
	wide.key("space")
	wide.autopilot(30 * 5)
	save(wide, "play-220x45-letterbox", canvas.TrueColor)
	tall := newHarness(t, 60, 50)
	tall.key("space")
	tall.autopilot(30 * 5)
	save(tall, "play-60x50-tall", canvas.TrueColor)
}
