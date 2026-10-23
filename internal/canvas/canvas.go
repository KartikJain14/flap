package canvas

import (
	"unicode"

	"github.com/mattn/go-runewidth"
)

// Attr is a set of text attributes.
type Attr uint8

const (
	Bold Attr = 1 << iota
	Dim
	Italic
	Underline
)

// Cell is one terminal cell. A Ch of 0 marks the right half of a wide rune.
type Cell struct {
	Ch   rune
	Fg   RGB
	Bg   RGB
	Attr Attr
}

// Rect is a cell rectangle.
type Rect struct{ X, Y, W, H int }

// Inset shrinks the rectangle by d on every side.
func (r Rect) Inset(d int) Rect { return Rect{r.X + d, r.Y + d, r.W - 2*d, r.H - 2*d} }

// Empty reports whether the rectangle has no area.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Centered returns a w×h rectangle centred inside r.
func (r Rect) Centered(w, h int) Rect {
	return Rect{r.X + (r.W-w)/2, r.Y + (r.H-h)/2, w, h}
}

// Canvas is a fixed-size grid of cells. All drawing is clipped to its bounds.
type Canvas struct {
	w, h  int
	cells []Cell
}

// New returns a canvas of the given size.
func New(w, h int) *Canvas {
	c := &Canvas{}
	c.Resize(w, h)
	return c
}

// Resize changes the canvas size, reusing memory where possible. The content
// is undefined afterwards.
func (c *Canvas) Resize(w, h int) {
	w, h = max(w, 0), max(h, 0)
	c.w, c.h = w, h
	if n := w * h; cap(c.cells) >= n {
		c.cells = c.cells[:n]
	} else {
		c.cells = make([]Cell, n)
	}
}

// Width returns the canvas width in cells.
func (c *Canvas) Width() int { return c.w }

// Height returns the canvas height in cells.
func (c *Canvas) Height() int { return c.h }

// Bounds returns the canvas rectangle.
func (c *Canvas) Bounds() Rect { return Rect{0, 0, c.w, c.h} }

// At returns the cell at (x, y), or the zero cell when out of bounds.
func (c *Canvas) At(x, y int) Cell {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return Cell{}
	}
	return c.cells[y*c.w+x]
}

// Set replaces the cell at (x, y) when it is within bounds.
func (c *Canvas) Set(x, y int, cell Cell) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	c.cells[y*c.w+x] = cell
}

func (c *Canvas) ref(x, y int) *Cell {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return nil
	}
	return &c.cells[y*c.w+x]
}

// Clear fills the whole canvas with blank cells of the given background.
func (c *Canvas) Clear(bg RGB) {
	blank := Cell{Ch: ' ', Fg: bg, Bg: bg}
	for i := range c.cells {
		c.cells[i] = blank
	}
}

// Fill paints a rectangle with blank cells of the given background.
func (c *Canvas) Fill(r Rect, bg RGB) {
	c.FillCell(r, Cell{Ch: ' ', Fg: bg, Bg: bg})
}

// FillCell paints a rectangle with copies of cell.
func (c *Canvas) FillCell(r Rect, cell Cell) {
	x0, y0 := max(r.X, 0), max(r.Y, 0)
	x1, y1 := min(r.X+r.W, c.w), min(r.Y+r.H, c.h)
	for y := y0; y < y1; y++ {
		row := c.cells[y*c.w : y*c.w+c.w]
		for x := x0; x < x1; x++ {
			row[x] = cell
		}
	}
}

// Shade multiplies the colours inside a rectangle by f, used for shadows and
// modal dimming.
func (c *Canvas) Shade(r Rect, f float64) {
	x0, y0 := max(r.X, 0), max(r.Y, 0)
	x1, y1 := min(r.X+r.W, c.w), min(r.Y+r.H, c.h)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			cell := &c.cells[y*c.w+x]
			cell.Fg = cell.Fg.Scale(f)
			cell.Bg = cell.Bg.Scale(f)
		}
	}
}

// Tint blends the colours inside a rectangle towards col by t.
func (c *Canvas) Tint(r Rect, col RGB, t float64) {
	x0, y0 := max(r.X, 0), max(r.Y, 0)
	x1, y1 := min(r.X+r.W, c.w), min(r.Y+r.H, c.h)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			cell := &c.cells[y*c.w+x]
			cell.Fg = cell.Fg.Lerp(col, t)
			cell.Bg = cell.Bg.Lerp(col, t)
		}
	}
}

// widthCond measures runes with ambiguous East Asian width as one column,
// independent of the server's locale.
var widthCond = &runewidth.Condition{EastAsianWidth: false}

// RuneWidth returns the number of columns r occupies: 0, 1 or 2. Runes that
// are unsafe to send to a terminal report 1 because they are replaced.
func RuneWidth(r rune) int {
	if fastRune(r) {
		return 1
	}
	if !SafeRune(r) {
		return 1
	}
	return widthCond.RuneWidth(r)
}

// fastRune reports whether r is a common rune that is known to be safe and
// one column wide: printable ASCII, box drawing and block elements.
func fastRune(r rune) bool {
	return r >= 0x20 && r < 0x7f || r >= 0x2500 && r <= 0x259f
}

// glyphWidth is RuneWidth for runes that are already known to be safe.
func glyphWidth(r rune) int {
	if fastRune(r) {
		return 1
	}
	return widthCond.RuneWidth(r)
}

// SafeRune reports whether r can be written to a terminal verbatim. Control
// characters (C0, DEL, C1), bidirectional overrides, zero-width and other
// format characters, surrogates and private-use runes are unsafe.
func SafeRune(r rune) bool {
	if fastRune(r) {
		return true
	}
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
		return false
	case r == 0xfffd:
		return true
	case r > unicode.MaxRune, r >= 0xd800 && r <= 0xdfff:
		return false
	case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r):
		return false
	case unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
		return false
	}
	return unicode.IsPrint(r) || r == ' '
}

// StringWidth returns the display width of s as Text would draw it.
func StringWidth(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// Truncate shortens s to at most w columns, appending an ellipsis when it
// had to cut.
func Truncate(s string, w int) string {
	if StringWidth(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	out := make([]rune, 0, w)
	n := 0
	for _, r := range s {
		rw := RuneWidth(r)
		if n+rw > w-1 {
			break
		}
		out = append(out, r)
		n += rw
	}
	return string(append(out, '…'))
}

// Text draws s starting at (x, y) with the given foreground and attributes,
// keeping each cell's background. Unsafe runes are replaced, zero-width runes
// are dropped and wide runes occupy two cells. It returns the number of
// columns advanced, including clipped ones.
func (c *Canvas) Text(x, y int, s string, fg RGB, attr Attr) int {
	return c.text(x, y, s, fg, nil, attr)
}

// TextBg is like Text but also sets the background.
func (c *Canvas) TextBg(x, y int, s string, fg, bg RGB, attr Attr) int {
	return c.text(x, y, s, fg, &bg, attr)
}

func (c *Canvas) text(x, y int, s string, fg RGB, bg *RGB, attr Attr) int {
	start := x
	for _, r := range s {
		if !SafeRune(r) {
			r = '?'
		}
		w := glyphWidth(r)
		if w == 0 {
			continue
		}
		if w == 2 && x+1 >= c.w {
			// A wide rune cannot be split at the right edge.
			r, w = ' ', 1
		}
		// Writing over the right half of a wide rune breaks that rune.
		if cell := c.ref(x, y); cell != nil && cell.Ch == 0 {
			if left := c.ref(x-1, y); left != nil {
				left.Ch = ' '
			}
		}
		for i := 0; i < w; i++ {
			cell := c.ref(x+i, y)
			if cell == nil {
				continue
			}
			b := cell.Bg
			if cell.Ch == upperHalf {
				// Text replaces a two-pixel cell; average the pixels.
				b = cell.Fg.Lerp(cell.Bg, 0.5)
			}
			if bg != nil {
				b = *bg
			}
			ch := r
			if i == 1 {
				ch = 0
			}
			*cell = Cell{Ch: ch, Fg: fg, Bg: b, Attr: attr}
		}
		// Writing over the left half of a wide rune orphans its right half.
		if cell := c.ref(x+w, y); cell != nil && cell.Ch == 0 {
			cell.Ch = ' '
		}
		x += w
	}
	return x - start
}

// TextCentered draws s horizontally centred in [x, x+w).
func (c *Canvas) TextCentered(x, y, w int, s string, fg RGB, attr Attr) {
	c.Text(x+(w-StringWidth(s))/2, y, s, fg, attr)
}

// TextRight draws s so that it ends just before column x.
func (c *Canvas) TextRight(x, y int, s string, fg RGB, attr Attr) {
	c.Text(x-StringWidth(s), y, s, fg, attr)
}

// BoxStyle is the set of runes used to draw a frame.
type BoxStyle struct {
	TL, TR, BL, BR, H, V rune
}

var (
	// RoundBox uses rounded box-drawing characters.
	RoundBox = BoxStyle{'╭', '╮', '╰', '╯', '─', '│'}
	// HeavyBox uses heavy box-drawing characters.
	HeavyBox = BoxStyle{'┏', '┓', '┗', '┛', '━', '┃'}
	// ASCIIBox uses only ASCII characters.
	ASCIIBox = BoxStyle{'+', '+', '+', '+', '-', '|'}
)

// Box draws a frame along the edge of r, keeping existing backgrounds unless
// bg is non-nil.
func (c *Canvas) Box(r Rect, st BoxStyle, fg RGB, bg *RGB) {
	if r.W < 2 || r.H < 2 {
		return
	}
	put := func(x, y int, ch rune) {
		cell := c.ref(x, y)
		if cell == nil {
			return
		}
		b := cell.Bg
		if bg != nil {
			b = *bg
		}
		*cell = Cell{Ch: ch, Fg: fg, Bg: b}
	}
	x1, y1 := r.X+r.W-1, r.Y+r.H-1
	for x := r.X + 1; x < x1; x++ {
		put(x, r.Y, st.H)
		put(x, y1, st.H)
	}
	for y := r.Y + 1; y < y1; y++ {
		put(r.X, y, st.V)
		put(x1, y, st.V)
	}
	put(r.X, r.Y, st.TL)
	put(x1, r.Y, st.TR)
	put(r.X, y1, st.BL)
	put(x1, y1, st.BR)
}

// HLine draws a horizontal run of ch.
func (c *Canvas) HLine(x, y, w int, ch rune, fg RGB) {
	for i := 0; i < w; i++ {
		if cell := c.ref(x+i, y); cell != nil {
			*cell = Cell{Ch: ch, Fg: fg, Bg: cell.Bg}
		}
	}
}
