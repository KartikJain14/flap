package tui

import "math"

// View width limits in world units. Clamping the visible width keeps the
// look-ahead comparable across terminal shapes: very wide terminals are
// letterboxed instead of revealing many more pipes, and tall narrow ones are
// shortened so pipes do not appear without warning.
const (
	minViewWidth = 1.35
	maxViewWidth = 2.2
)

// layout maps the normalised world onto terminal cells and pixels.
type layout struct {
	w, h     int
	tooSmall bool

	ppc        int // pixel rows per cell: 2 with half blocks, 1 in ASCII mode
	groundRows int
	skyRows    int

	// The play area is where pipes and the bird live. It sits directly on
	// the ground and may be narrower or shorter than the sky.
	playTop, playRows int
	playX, playW      int

	unitY float64 // pixel rows per world unit
	unitX float64 // columns per world unit
	viewW float64 // world units visible across the play area
}

func computeLayout(w, h, minW, minH int, ascii bool) layout {
	l := layout{w: w, h: h, ppc: 2}
	if ascii {
		l.ppc = 1
	}
	if w < minW || h < minH || w <= 0 || h <= 0 {
		l.tooSmall = true
		return l
	}
	switch {
	case h < 24:
		l.groundRows = 2
	case h < 36:
		l.groundRows = 3
	default:
		l.groundRows = 4
	}
	l.skyRows = h - l.groundRows
	l.playRows = l.skyRows
	// Cells are about twice as tall as wide, so a world unit (one play
	// height) spans 2*playRows columns.
	if float64(w)/float64(2*l.playRows) < minViewWidth {
		l.playRows = max(1, int(math.Floor(float64(w)/(2*minViewWidth))))
	}
	l.playTop = l.skyRows - l.playRows
	l.unitX = float64(2 * l.playRows)
	l.unitY = float64(l.playRows * l.ppc)
	l.playW = w
	if float64(w)/l.unitX > maxViewWidth {
		l.playW = int(math.Floor(maxViewWidth * l.unitX))
		l.playX = (w - l.playW) / 2
	}
	l.viewW = float64(l.playW) / l.unitX
	return l
}

// pxW and pxH are the dimensions of the full-screen pixel bitmap.
func (l layout) pxW() int { return l.w }
func (l layout) pxH() int { return l.h * l.ppc }

// groundTop is the first pixel row of the ground.
func (l layout) groundTop() int { return l.skyRows * l.ppc }

// worldY converts a world y coordinate to a pixel row.
func (l layout) worldY(y float64) float64 { return float64(l.playTop*l.ppc) + y*l.unitY }

// worldX converts a world x coordinate relative to the camera to a column.
func (l layout) worldX(x float64) float64 { return float64(l.playX) + x*l.unitX }

// letterboxed reports whether the play area is narrower than the screen.
func (l layout) letterboxed() bool { return l.playW < l.w }
