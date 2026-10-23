package tui

import (
	"bytes"
	"testing"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"

	"flap/internal/canvas"
)

// measureWire plays for a while and returns the average number of bytes
// per second that Bubble Tea's terminal renderer (the same diffing renderer
// used in production) would send, at the adaptive gameplay frame rate.
func measureWire(t *testing.T, w, h int, mode canvas.ColorMode) (bytesPerFrame, kbps float64, fps int) {
	t.Helper()
	hs := newHarness(t, w, h, func(s *Session, o *Options) { s.Color = mode })
	var out bytes.Buffer
	r := uv.NewTerminalRenderer(&out, []string{"TERM=xterm-256color"})
	r.SetFullscreen(true)
	r.SetRelativeCursor(false)
	r.SetColorProfile(colorprofile.TrueColor)
	r.EnterAltScreen()
	buf := uv.NewScreenBuffer(w, h)
	fps = PlayFPS(30, w, h)
	hs.key("space")
	total, frames := 0, 0
	for f := 0; f < 10*fps; f++ {
		hs.frames(1, fps)
		if wantsFlap(hs.app.world) {
			hs.key("space")
		}
		if hs.app.screen != screenPlay {
			hs.app.overAt = -10
			hs.key("space")
		}
		v := hs.app.View()
		buf.Clear()
		uv.NewStyledString(v.Content).Draw(buf, buf.Bounds())
		out.Reset()
		r.Render(buf.RenderBuffer)
		if err := r.Flush(); err != nil {
			t.Fatal(err)
		}
		if f >= fps { // skip the initial full paint
			total += out.Len()
			frames++
		}
	}
	bytesPerFrame = float64(total) / float64(frames)
	return bytesPerFrame, bytesPerFrame * float64(fps) / 1024, fps
}

// TestBandwidthBudget guards against rendering changes that would make the
// game expensive to stream over SSH. The budgets are for active gameplay in
// 256-colour mode (the default for xterm-256color clients).
func TestBandwidthBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	budgets := map[[2]int]float64{ // KB/s
		{80, 24}: 100, {100, 30}: 110, {120, 40}: 140, {160, 50}: 170,
	}
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {120, 40}, {160, 50}} {
		for _, mode := range []canvas.ColorMode{canvas.Color256, canvas.TrueColor} {
			bpf, kbps, fps := measureWire(t, sz[0], sz[1], mode)
			t.Logf("%dx%d %-9v %2d fps: %5.0f bytes/frame, %5.1f KB/s", sz[0], sz[1], mode, fps, bpf, kbps)
			if mode == canvas.Color256 && kbps > budgets[sz] {
				t.Errorf("%dx%d uses %.1f KB/s, budget %.0f", sz[0], sz[1], kbps, budgets[sz])
			}
		}
	}
}

func TestPlayFPS(t *testing.T) {
	cases := []struct{ max, w, h, want int }{
		{30, 50, 18, 30}, {30, 80, 24, 30}, {30, 100, 30, 24}, {30, 160, 50, 18}, {30, 400, 200, 18}, {12, 400, 200, 12},
	}
	for _, c := range cases {
		if got := PlayFPS(c.max, c.w, c.h); got != c.want {
			t.Errorf("PlayFPS(%d, %d, %d) = %d, want %d", c.max, c.w, c.h, got, c.want)
		}
	}
}
