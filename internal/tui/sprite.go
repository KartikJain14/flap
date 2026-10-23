package tui

import (
	"math"

	"flap/internal/canvas"
)

// The bird is described with simple shapes in "bird units" (the body is about
// 1.6 units wide) and rasterised on demand at whatever size the terminal
// needs. Supersampling with a majority vote keeps edges crisp like pixel art
// instead of blurring them.

type material uint8

const (
	matNone material = iota
	matOutline
	matBody
	matShade
	matBelly
	matWing
	matWingEdge
	matEye
	matPupil
	matGlint
	matBeak
	matBeakLow
	matCheek
)

// skin is a bird colour scheme. Each player gets a stable skin.
type skin struct {
	name                        string
	body, shade, belly, cheek   rgb
	wing, wingEdge, beak, beak2 rgb
}

var skins = []skin{
	{"Sunny", hex(0xffd43b), hex(0xf0a020), hex(0xfff2ad), hex(0xff9e7a), hex(0xfffaf0), hex(0xe9d199), hex(0xff8a33), hex(0xdd5428)},
	{"Ocean", hex(0x5cc8ff), hex(0x2f8fe0), hex(0xd4f2ff), hex(0xff9eb8), hex(0xf4fbff), hex(0xa9d4f2), hex(0xffb03a), hex(0xe07a1f)},
	{"Berry", hex(0xff6f91), hex(0xd9416a), hex(0xffd2dd), hex(0xffb08a), hex(0xfff5f7), hex(0xf2b3c4), hex(0xffc13a), hex(0xe09020)},
	{"Mint", hex(0x7be495), hex(0x3fb563), hex(0xdcfbe3), hex(0xff9e7a), hex(0xf6fff8), hex(0xb3e6c0), hex(0xff8a33), hex(0xdd5428)},
	{"Plum", hex(0xb98cff), hex(0x8456e0), hex(0xece0ff), hex(0xffa0c8), hex(0xfbf7ff), hex(0xcdb8f2), hex(0xffc13a), hex(0xe09020)},
}

var (
	colOutline = hex(0x3a2412)
	colEye     = hex(0xffffff)
	colPupil   = hex(0x1a1a2a)
)

func (s *skin) color(m material) rgb {
	switch m {
	case matOutline:
		return colOutline
	case matBody:
		return s.body
	case matShade:
		return s.shade
	case matBelly:
		return s.belly
	case matCheek:
		return s.cheek
	case matWing:
		return s.wing
	case matWingEdge:
		return s.wingEdge
	case matEye, matGlint:
		return colEye
	case matPupil:
		return colPupil
	case matBeak:
		return s.beak
	case matBeakLow:
		return s.beak2
	}
	return rgb{}
}

func inEllipse(x, y, cx, cy, rx, ry float64) bool {
	dx, dy := (x-cx)/rx, (y-cy)/ry
	return dx*dx+dy*dy <= 1
}

// birdMaterial returns the material at (x, y) in bird space, x to the right
// (the bird faces right) and y downwards. wing is -1 (up) to 1 (down).
func birdMaterial(x, y, wing float64, detail bool) material {
	// Eye: a large white eye with a pupil looking forward.
	if detail && inEllipse(x, y, 0.47, -0.30, 0.055, 0.055) {
		return matGlint
	}
	if inEllipse(x, y, 0.52, -0.2, 0.13, 0.15) {
		return matPupil
	}
	if inEllipse(x, y, 0.38, -0.22, 0.30, 0.31) {
		return matEye
	}
	// Beak: a short wedge split into upper and lower halves.
	if x >= 0.55 && x <= 1.1 {
		t := (x - 0.55) / 0.55
		top := -0.02 + t*0.18
		bot := 0.40 - t*0.22
		if y >= top && y <= bot {
			if y < (top+bot)/2+0.02 {
				return matBeak
			}
			return matBeakLow
		}
	}
	// Wing: an ellipse that moves with the flap phase.
	wy := 0.16 + 0.24*wing
	if inEllipse(x, y, -0.36, wy, 0.46, 0.25-0.05*math.Abs(wing)) {
		if y > wy+0.12 || x < -0.72 {
			return matWingEdge
		}
		return matWing
	}
	// Head tuft.
	if inEllipse(x, y, -0.06, -0.80, 0.09, 0.15) || inEllipse(x, y, 0.1, -0.76, 0.07, 0.12) {
		return matShade
	}
	// Tail.
	if x < -0.68 && x > -1.08 {
		half := (x + 1.08) * 0.55
		if math.Abs(y-0.02) < 0.24-half*0.4 && math.Abs(y-0.02) < 0.05+half {
			return matShade
		}
	}
	// Body with belly, cheek and a darker lower back.
	if inEllipse(x, y, 0, 0.04, 0.8, 0.72) {
		switch {
		case detail && inEllipse(x, y, 0.42, 0.18, 0.12, 0.08):
			return matCheek
		case inEllipse(x, y, 0.2, 0.42, 0.52, 0.34):
			return matBelly
		case y > 0.38 || (x < -0.45 && y > 0.1):
			return matShade
		}
		return matBody
	}
	return matNone
}

// sprite is a rasterised bird. A pixel is transparent when its mask is false.
type sprite struct {
	w, h int
	pix  []rgb
	mask []bool
}

type spriteKey struct {
	scaleX, scaleY int // quarter-pixel units per bird unit
	wing           int // -1, 0, 1
	angle          int // degrees, quantised
	skin           int
}

// rasterBird renders the bird at scaleX columns and scaleY pixel rows per
// bird unit, rotated by angle (degrees, positive = nose down).
func rasterBird(scaleX, scaleY float64, wing float64, angle float64, sk *skin) *sprite {
	const span = 2.5 // bird units covered by the sprite box, with room to rotate
	w := max(3, int(math.Ceil(span*scaleX)))
	h := max(3, int(math.Ceil(span*scaleY)))
	sp := &sprite{w: w, h: h, pix: make([]rgb, w*h), mask: make([]bool, w*h)}
	mats := make([]material, w*h)
	detail := scaleY >= 7
	sin, cos := math.Sincos(-angle * math.Pi / 180)
	const ss = 3
	var counts [matCheek + 1]int
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			counts = [matCheek + 1]int{}
			for sj := 0; sj < ss; sj++ {
				for si := 0; si < ss; si++ {
					// Sample position in bird space, then undo the rotation.
					x := (float64(i)+(float64(si)+0.5)/ss)/scaleX - span/2
					y := (float64(j)+(float64(sj)+0.5)/ss)/scaleY - span/2
					rx, ry := x*cos-y*sin, x*sin+y*cos
					counts[birdMaterial(rx, ry, wing, detail)]++
				}
			}
			best, bestN := matNone, 0
			filled := ss*ss - counts[matNone]
			if filled*2 >= ss*ss {
				for m := matOutline; m <= matCheek; m++ {
					// Small features win ties so eyes survive at low sizes.
					if counts[m] > bestN || (counts[m] == bestN && counts[m] > 0 && m > best) {
						best, bestN = m, counts[m]
					}
				}
				// Keep the pupil visible whenever it covers a third of a pixel.
				if counts[matPupil]*3 >= ss*ss {
					best = matPupil
				}
			}
			mats[j*w+i] = best
		}
	}
	// A dark outline around the silhouette once the sprite is big enough.
	if scaleY >= 3.2 {
		out := append([]material(nil), mats...)
		for j := 0; j < h; j++ {
			for i := 0; i < w; i++ {
				if mats[j*w+i] != matNone {
					continue
				}
				for _, d := range [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					x, y := i+d[0], j+d[1]
					if x >= 0 && y >= 0 && x < w && y < h && mats[y*w+x] != matNone {
						out[j*w+i] = matOutline
						break
					}
				}
			}
		}
		mats = out
	}
	for k, m := range mats {
		if m != matNone {
			sp.mask[k] = true
			sp.pix[k] = sk.color(m)
		}
	}
	return sp
}

// blit draws the sprite centred at (cx, cy) in pixel coordinates.
func (s *sprite) blit(px *canvas.Pixels, cx, cy float64, tint rgb, tintAmt float64) {
	x0 := int(math.Round(cx - float64(s.w)/2))
	y0 := int(math.Round(cy - float64(s.h)/2))
	for j := 0; j < s.h; j++ {
		for i := 0; i < s.w; i++ {
			k := j*s.w + i
			if !s.mask[k] {
				continue
			}
			c := s.pix[k]
			if tintAmt > 0 {
				c = c.Lerp(tint, tintAmt)
			}
			px.Set(x0+i, y0+j, c)
		}
	}
}

// spriteCache memoises rasterised birds for one session. It is bounded so a
// session that keeps resizing cannot grow it forever.
type spriteCache struct {
	m map[spriteKey]*sprite
}

func (c *spriteCache) get(scaleX, scaleY, wing, angle float64, skinIdx int) *sprite {
	k := spriteKey{
		scaleX: int(math.Round(scaleX * 4)), scaleY: int(math.Round(scaleY * 4)),
		wing: int(math.Round(wing)), angle: int(math.Round(angle/8) * 8), skin: skinIdx,
	}
	if s, ok := c.m[k]; ok {
		return s
	}
	if c.m == nil || len(c.m) > 256 {
		c.m = make(map[spriteKey]*sprite)
	}
	s := rasterBird(float64(k.scaleX)/4, float64(k.scaleY)/4, float64(k.wing), float64(k.angle), &skins[skinIdx%len(skins)])
	c.m[k] = s
	return s
}
