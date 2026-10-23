package canvas

import (
	"regexp"
	"strings"
	"testing"
)

var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

// rows strips SGR sequences from an encoded canvas and splits it into rows.
func rows(t *testing.T, c *Canvas, mode ColorMode) []string {
	t.Helper()
	var b strings.Builder
	var buf = new(bytesBuffer)
	c.Encode(&buf.Buffer, mode)
	b.WriteString(buf.String())
	plain := sgr.ReplaceAllString(b.String(), "")
	for i, r := range plain {
		if r < 0x20 && r != '\n' || r == 0x7f || r >= 0x80 && r < 0xa0 {
			t.Fatalf("control character %U at byte %d in encoded output", r, i)
		}
	}
	return strings.Split(plain, "\n")
}

func TestEncodeRowsHaveExactWidth(t *testing.T) {
	c := New(12, 3)
	c.Clear(Hex(0x102030))
	c.Text(0, 0, "日本語テキスト", Hex(0xffffff), Bold)
	c.Text(3, 0, "x", Hex(0xff0000), 0)   // overwrite the right half of a wide rune
	c.Text(10, 1, "日本", Hex(0xffffff), 0) // wide rune clipped at the edge
	c.Text(0, 2, "a\u0301b\u200dc", Hex(0xffffff), 0)
	for _, mode := range []ColorMode{TrueColor, Color256, Color16} {
		for i, r := range rows(t, c, mode) {
			if w := StringWidth(r); w != 12 {
				t.Fatalf("mode %v row %d %q has width %d, want 12", mode, i, r, w)
			}
		}
	}
}

func TestTextNeutralisesEscapeInjection(t *testing.T) {
	evil := "\x1b[2J\x1b]0;pwned\x07\x9b31m\u202eevil\r\n\x00\u2066x"
	c := New(40, 1)
	c.Clear(Hex(0))
	c.Text(0, 0, evil, Hex(0xffffff), 0)
	var b bytesBuffer
	c.Encode(&b.Buffer, TrueColor)
	out := b.String()
	if strings.Contains(out, "pwned\x07") || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b]") {
		t.Fatalf("escape sequence leaked: %q", out)
	}
	if n := strings.Count(sgr.ReplaceAllString(out, ""), "\x1b"); n != 0 {
		t.Fatalf("%d raw ESC bytes leaked: %q", n, out)
	}
	for _, r := range []rune{'\u202e', '\u2066', '\x9b', '\r', '\x00'} {
		if strings.ContainsRune(out, r) {
			t.Fatalf("unsafe rune %U leaked", r)
		}
	}
}

func TestEncodeNeverEmitsUnsafeCellRunes(t *testing.T) {
	c := New(4, 1)
	c.Set(0, 0, Cell{Ch: 0x1b})
	c.Set(1, 0, Cell{Ch: '\u200b'})
	c.Set(2, 0, Cell{Ch: '\n'})
	c.Set(3, 0, Cell{Ch: 'o'})
	got := rows(t, c, TrueColor)[0]
	if got != "???o" {
		t.Fatalf("got %q", got)
	}
}

func TestSafeRune(t *testing.T) {
	for _, r := range []rune{'a', ' ', '█', '▀', '╭', 'é', '日', '✓'} {
		if !SafeRune(r) {
			t.Errorf("%U should be safe", r)
		}
	}
	for _, r := range []rune{0, 0x07, 0x1b, 0x7f, 0x85, 0x9b, '\u200b', '\u200e', '\u202e', '\u2066', '\ufeff', '\ue000', '\u2028'} {
		if SafeRune(r) {
			t.Errorf("%U should be unsafe", r)
		}
	}
}

func TestDrawPixelsHalfBlocks(t *testing.T) {
	var p Pixels
	p.Resize(2, 4)
	red, blue := Hex(0xff0000), Hex(0x0000ff)
	p.FillRect(0, 0, 2, 4, blue)
	p.Set(0, 0, red)
	c := New(2, 2)
	c.DrawPixels(&p, 0, 0, 2)
	if got := c.At(0, 0); got.Ch != '▀' || got.Fg != red || got.Bg != blue {
		t.Fatalf("cell (0,0) = %+v", got)
	}
	if got := c.At(1, 0); got.Ch != ' ' || got.Bg != blue {
		t.Fatalf("cell (1,0) = %+v", got)
	}
	c.DrawPixels(&p, 0, 0, 1)
	if got := c.At(0, 0); got.Ch != ' ' || got.Bg != red {
		t.Fatalf("ascii cell (0,0) = %+v", got)
	}
}

func TestDrawingIsClipped(t *testing.T) {
	c := New(5, 3)
	c.Clear(Hex(0))
	c.Fill(Rect{-10, -10, 100, 100}, Hex(0x111111))
	c.Box(Rect{-1, -1, 10, 10}, RoundBox, Hex(0xffffff), nil)
	c.Text(-3, 1, "hello world", Hex(0xffffff), 0)
	c.Text(2, 99, "x", Hex(0xffffff), 0)
	var p Pixels
	p.Resize(20, 20)
	c.DrawPixels(&p, -4, -4, 2)
	p.Disc(5, 5, 30, 1, Hex(0xffffff), 2)
	p.FillRect(-5, -5, 50, 50, Hex(0x222222))
	if got := len(rows(t, c, TrueColor)); got != 3 {
		t.Fatalf("got %d rows", got)
	}
}

func TestTruncate(t *testing.T) {
	cases := map[string]struct {
		in   string
		w    int
		want string
	}{
		"fits":  {"hello", 5, "hello"},
		"cut":   {"hello world", 6, "hello…"},
		"wide":  {"日本語", 4, "日…"},
		"zero":  {"abc", 0, ""},
		"exact": {"日本", 4, "日本"},
	}
	for name, c := range cases {
		if got := Truncate(c.in, c.w); got != c.want {
			t.Errorf("%s: Truncate(%q, %d) = %q, want %q", name, c.in, c.w, got, c.want)
		}
	}
}

func TestColorQuantization(t *testing.T) {
	if got := To256(Hex(0xff0000)); got != 196 {
		t.Errorf("red -> %d, want 196", got)
	}
	if got := To256(Hex(0x000000)); got != 16 {
		t.Errorf("black -> %d, want 16", got)
	}
	if got := To256(Hex(0x808080)); got < 232 {
		t.Errorf("grey -> %d, want grey ramp", got)
	}
	if got := To16(Hex(0x00ff00)); got != 2 && got != 10 {
		t.Errorf("green -> %d, want a green (2 or 10)", got)
	}
	if got := To16(Hex(0xffffff)); got != 15 {
		t.Errorf("white -> %d, want 15", got)
	}
	// Quantisation must be stable: quantising a quantised colour is a no-op.
	for _, c := range []RGB{Hex(0x70c5ce), Hex(0xde5a3c), Hex(0x123456), Hex(0xfafafa)} {
		q := Quantize(c, Color256)
		if Quantize(q, Color256) != q {
			t.Errorf("256 quantisation of %v not idempotent", c)
		}
	}
}

func TestEncodeModes(t *testing.T) {
	c := New(2, 1)
	c.Set(0, 0, Cell{Ch: 'a', Fg: Hex(0xff0000), Bg: Hex(0x0000ff)})
	c.Set(1, 0, Cell{Ch: 'b', Fg: Hex(0xff0000), Bg: Hex(0x0000ff)})
	var b bytesBuffer
	c.Encode(&b.Buffer, Color256)
	if s := b.String(); !strings.Contains(s, "38;5;196") || !strings.Contains(s, "48;5;21") {
		t.Fatalf("256 encoding: %q", s)
	}
	b.Reset()
	c.Encode(&b.Buffer, Color16)
	if s := b.String(); !regexp.MustCompile(`^\x1b\[0;(4[0-7]|10[0-7]);(3[0-7]|9[0-7])mab\x1b\[m$`).MatchString(s) {
		t.Fatalf("16 encoding: %q", s)
	}
	b.Reset()
	c.Encode(&b.Buffer, TrueColor)
	// Same style twice must not repeat the SGR sequence.
	if n := strings.Count(b.String(), "38;2;255;0;0"); n != 1 {
		t.Fatalf("foreground emitted %d times: %q", n, b.String())
	}
}

func TestPlainText(t *testing.T) {
	c := New(6, 2)
	c.Clear(Hex(0))
	c.Text(1, 0, "hi", Hex(0xffffff), 0)
	if got := c.PlainText(); got != " hi\n" {
		t.Fatalf("PlainText = %q", got)
	}
}

func BenchmarkEncode160x50(b *testing.B) {
	c := New(160, 50)
	var p Pixels
	p.Resize(160, 100)
	for y := 0; y < 100; y++ {
		for x := 0; x < 160; x++ {
			p.Set(x, y, Hex(0x4060a0).Lerp(Hex(0xa0e0ff), float64(y)/100))
		}
	}
	var buf bytesBuffer
	b.ReportAllocs()
	for b.Loop() {
		c.DrawPixels(&p, 0, 0, 2)
		buf.Reset()
		c.Encode(&buf.Buffer, TrueColor)
	}
	b.ReportMetric(float64(buf.Len()), "bytes/frame")
}
