// Package canvas provides a terminal cell grid with a half-block pixel layer
// and a compact ANSI encoder.
//
// Everything drawn to the terminal goes through a Canvas. The encoder is the
// single place where runes become bytes, and it refuses to emit control
// characters, so player-controlled text can never inject escape sequences.
package canvas

import "math"

// RGB is a 24-bit sRGB colour.
type RGB struct{ R, G, B uint8 }

// Hex builds a colour from a 0xRRGGBB literal.
func Hex(v uint32) RGB { return RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)} }

// Lerp interpolates linearly from c to d; t is clamped to [0, 1].
func (c RGB) Lerp(d RGB, t float64) RGB {
	if t <= 0 {
		return c
	}
	if t >= 1 {
		return d
	}
	l := func(a, b uint8) uint8 { return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t)) }
	return RGB{l(c.R, d.R), l(c.G, d.G), l(c.B, d.B)}
}

// Scale multiplies every channel by f, clamping to the valid range.
func (c RGB) Scale(f float64) RGB {
	s := func(a uint8) uint8 { return uint8(math.Max(0, math.Min(255, math.Round(float64(a)*f)))) }
	return RGB{s(c.R), s(c.G), s(c.B)}
}

// Luma returns the approximate perceived brightness in [0, 1].
func (c RGB) Luma() float64 {
	return (0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)) / 255
}

// ColorMode selects how colours are encoded.
type ColorMode uint8

const (
	// TrueColor emits 24-bit colours.
	TrueColor ColorMode = iota
	// Color256 emits xterm 256-colour palette indices.
	Color256
	// Color16 emits the basic 16 ANSI colours.
	Color16
)

func (m ColorMode) String() string {
	switch m {
	case TrueColor:
		return "truecolor"
	case Color256:
		return "256"
	case Color16:
		return "16"
	}
	return "unknown"
}

// ParseColorMode parses the String form of a ColorMode.
func ParseColorMode(s string) (ColorMode, bool) {
	switch s {
	case "truecolor", "24bit":
		return TrueColor, true
	case "256":
		return Color256, true
	case "16":
		return Color16, true
	}
	return 0, false
}

// Next cycles through the colour modes.
func (m ColorMode) Next() ColorMode { return (m + 1) % 3 }

// cubeLevels are the channel intensities of the xterm 6×6×6 colour cube.
var cubeLevels = [6]int{0, 95, 135, 175, 215, 255}

func nearestCube(v uint8) int {
	// Thresholds halfway between cube levels.
	switch {
	case v < 48:
		return 0
	case v < 115:
		return 1
	}
	return min(5, (int(v)-35)/40)
}

// dist is a weighted squared distance that tracks perceived differences
// better than plain RGB distance ("redmean").
func dist(a RGB, r, g, b int) int {
	rm := (int(a.R) + r) / 2
	dr, dg, db := int(a.R)-r, int(a.G)-g, int(a.B)-b
	return ((512+rm)*dr*dr)>>8 + 4*dg*dg + ((767-rm)*db*db)>>8
}

// To256 maps a colour to the nearest xterm 256-colour index, considering the
// colour cube and the grey ramp (the first 16 indices vary per terminal and
// are deliberately avoided).
func To256(c RGB) uint8 {
	ri, gi, bi := nearestCube(c.R), nearestCube(c.G), nearestCube(c.B)
	cube := 16 + 36*ri + 6*gi + bi
	cd := dist(c, cubeLevels[ri], cubeLevels[gi], cubeLevels[bi])

	avg := (int(c.R) + int(c.G) + int(c.B)) / 3
	gi2 := 23
	if avg < 238 {
		gi2 = max(0, (avg-3)/10)
	}
	gv := 8 + 10*gi2
	gd := dist(c, gv, gv, gv)
	if gd < cd {
		return uint8(232 + gi2)
	}
	return uint8(cube)
}

// ansi16 approximates the common default 16-colour palette. Actual
// terminals differ, so To16 matches by hue family rather than by exact RGB
// distance: a light sky should become cyan, not grey, whatever the palette.
var ansi16 = [16]RGB{
	{0, 0, 0}, {205, 49, 49}, {13, 188, 121}, {229, 179, 16},
	{36, 114, 200}, {188, 63, 188}, {17, 168, 205}, {204, 204, 204},
	{102, 102, 102}, {241, 76, 76}, {35, 209, 139}, {245, 230, 90},
	{59, 142, 234}, {214, 112, 214}, {41, 200, 230}, {255, 255, 255},
}

// To16 maps a colour to a basic ANSI colour index (0–15) by hue family and
// lightness.
func To16(c RGB) uint8 {
	r, g, b := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l := (mx + mn) / 2
	d := mx - mn
	var s float64
	if d > 0 {
		s = d / (1 - math.Abs(2*l-1))
	}
	if s < 0.2 || d < 0.08 {
		switch {
		case l < 0.18:
			return 0
		case l < 0.45:
			return 8
		case l < 0.82:
			return 7
		}
		return 15
	}
	var h float64
	switch mx {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	var base uint8
	bright := 0.62
	switch {
	case h < 18 || h >= 330:
		base = 1 // red
	case h < 68:
		base, bright = 3, 0.7 // yellow, orange and browns
	case h < 165:
		base = 2 // green
	case h < 215:
		base = 6 // cyan: skies
	case h < 265:
		base = 4 // blue
	default:
		base = 5 // magenta
	}
	if l < 0.12 {
		return 0
	}
	if l > bright {
		return base + 8
	}
	return base
}

// Quantize returns the colour a terminal in the given mode will display,
// as an RGB approximation. It is used for previews and tests.
func Quantize(c RGB, m ColorMode) RGB {
	switch m {
	case Color256:
		i := int(To256(c))
		if i >= 232 {
			v := uint8(8 + 10*(i-232))
			return RGB{v, v, v}
		}
		i -= 16
		return RGB{uint8(cubeLevels[i/36]), uint8(cubeLevels[i/6%6]), uint8(cubeLevels[i%6])}
	case Color16:
		return ansi16[To16(c)]
	}
	return c
}
