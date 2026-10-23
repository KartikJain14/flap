package canvas

import (
	"bytes"
	"strconv"
	"unicode/utf8"
)

// pen is the SGR state the encoder believes the terminal is in.
type pen struct {
	valid  bool
	fg, bg RGB
	attr   Attr
	fgSet  bool
}

// Encode serialises the canvas as rows of styled text separated by "\n".
// Each row ends with an SGR reset so rows are independent. SGR sequences are
// only emitted when the style changes, and the foreground of blank cells is
// ignored, which keeps frames small.
//
// Every rune is passed through SafeRune; anything unsafe becomes '?'. This is
// the last line of defence against terminal escape injection.
func (c *Canvas) Encode(buf *bytes.Buffer, mode ColorMode) {
	buf.Grow(c.w * c.h * 4)
	var tmp [64]byte
	for y := 0; y < c.h; y++ {
		var p pen
		row := c.cells[y*c.w : y*c.w+c.w]
		prevWide := false
		for x := 0; x < len(row); x++ {
			cell := row[x]
			ch := cell.Ch
			if ch == 0 {
				if prevWide {
					// Right half of a wide rune that was already written.
					prevWide = false
					continue
				}
				// An orphaned right half; keep the column count intact.
				ch = ' '
			}
			var wide bool
			if !fastRune(ch) {
				w := 1
				if SafeRune(ch) {
					w = glyphWidth(ch)
				} else {
					w = 0
				}
				wide = w == 2
				if w == 0 || (wide && (x+1 >= len(row) || row[x+1].Ch != 0)) {
					// Unsafe, zero-width, or a wide rune whose continuation
					// cell was overwritten: draw a one-column placeholder.
					ch, wide = '?', false
				}
			}
			prevWide = wide
			// Blank cells always use the default foreground. Their colour is
			// invisible, and giving visually identical cells identical styles
			// lets the terminal renderer's diff skip them between frames.
			needFg := ch != ' ' || cell.Attr&Underline != 0
			seq := tmp[:0]
			switch {
			case !p.valid || p.attr != cell.Attr:
				seq = append(seq, "\x1b[0"...)
				seq = appendAttrs(seq, cell.Attr)
				seq = append(seq, ';')
				seq = appendColor(seq, cell.Bg, mode, false)
				if needFg {
					seq = append(seq, ';')
					seq = appendColor(seq, cell.Fg, mode, true)
				}
				seq = append(seq, 'm')
				p = pen{valid: true, fg: cell.Fg, bg: cell.Bg, attr: cell.Attr, fgSet: needFg}
			default:
				bgChanged := !sameColor(p.bg, cell.Bg, mode)
				fgChanged := needFg && (!p.fgSet || !sameColor(p.fg, cell.Fg, mode))
				fgReset := !needFg && p.fgSet
				if bgChanged || fgChanged || fgReset {
					seq = append(seq, "\x1b["...)
					sep := false
					if bgChanged {
						seq = appendColor(seq, cell.Bg, mode, false)
						p.bg = cell.Bg
						sep = true
					}
					if sep && (fgChanged || fgReset) {
						seq = append(seq, ';')
					}
					switch {
					case fgChanged:
						seq = appendColor(seq, cell.Fg, mode, true)
						p.fg, p.fgSet = cell.Fg, true
					case fgReset:
						seq = append(seq, "39"...)
						p.fgSet = false
					}
					seq = append(seq, 'm')
				}
			}
			buf.Write(seq)
			if ch < utf8.RuneSelf {
				buf.WriteByte(byte(ch))
			} else {
				buf.WriteRune(ch)
			}
		}
		buf.WriteString("\x1b[m")
		if y < c.h-1 {
			buf.WriteByte('\n')
		}
	}
}

// String encodes the canvas in true colour; handy for tests.
func (c *Canvas) String() string {
	var b bytes.Buffer
	c.Encode(&b, TrueColor)
	return b.String()
}

// PlainText returns the canvas runes without styling, one line per row with
// trailing spaces trimmed.
func (c *Canvas) PlainText() string {
	var b bytes.Buffer
	for y := 0; y < c.h; y++ {
		line := make([]rune, 0, c.w)
		for x := 0; x < c.w; x++ {
			ch := c.cells[y*c.w+x].Ch
			switch {
			case ch == 0:
				continue
			case ch == upperHalf:
				ch = ' '
			}
			line = append(line, ch)
		}
		s := string(line)
		end := len(s)
		for end > 0 && s[end-1] == ' ' {
			end--
		}
		b.WriteString(s[:end])
		if y < c.h-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func sameColor(a, b RGB, mode ColorMode) bool {
	if a == b {
		return true
	}
	switch mode {
	case Color256:
		return To256(a) == To256(b)
	case Color16:
		return To16(a) == To16(b)
	}
	return false
}

func appendAttrs(b []byte, a Attr) []byte {
	if a&Bold != 0 {
		b = append(b, ";1"...)
	}
	if a&Dim != 0 {
		b = append(b, ";2"...)
	}
	if a&Italic != 0 {
		b = append(b, ";3"...)
	}
	if a&Underline != 0 {
		b = append(b, ";4"...)
	}
	return b
}

func appendColor(b []byte, c RGB, mode ColorMode, fg bool) []byte {
	switch mode {
	case Color256:
		if fg {
			b = append(b, "38;5;"...)
		} else {
			b = append(b, "48;5;"...)
		}
		return strconv.AppendUint(b, uint64(To256(c)), 10)
	case Color16:
		i := int(To16(c))
		base := 40
		if fg {
			base = 30
		}
		if i >= 8 {
			base += 60
			i -= 8
		}
		return strconv.AppendInt(b, int64(base+i), 10)
	default:
		if fg {
			b = append(b, "38;2;"...)
		} else {
			b = append(b, "48;2;"...)
		}
		b = strconv.AppendUint(b, uint64(c.R), 10)
		b = append(b, ';')
		b = strconv.AppendUint(b, uint64(c.G), 10)
		b = append(b, ';')
		return strconv.AppendUint(b, uint64(c.B), 10)
	}
}
