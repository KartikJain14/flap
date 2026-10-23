package tui

import (
	"math"
	"math/rand/v2"

	"flap/internal/canvas"
	"flap/internal/game"
)

// scene owns the pixel buffers used to draw the playfield. The sky is cached
// because it only changes when the size or theme changes, which keeps both
// CPU time and the per-frame diff small.
type scene struct {
	px  canvas.Pixels
	sky canvas.Pixels

	skyValid bool
	skyW     int
	skyH     int
	skyTheme theme
	skyPhase int

	stars   []star
	clouds  []cloud
	sprites spriteCache
}

type star struct {
	x, y    int
	bright  float64
	twinkle bool
	phase   float64
}

type cloud struct {
	x, y, r float64 // x in cloud-tile units, y as a fraction of sky height, r in world units
	puffs   int
}

const cloudTile = 3.2 // world units after which the cloud pattern repeats

func newScene() *scene {
	s := &scene{}
	rng := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 7; i++ {
		s.clouds = append(s.clouds, cloud{
			x:     float64(i)*cloudTile/7 + rng.Float64()*0.25,
			y:     0.08 + rng.Float64()*0.38,
			r:     0.035 + rng.Float64()*0.035,
			puffs: 3 + rng.IntN(3),
		})
	}
	return s
}

func (s *scene) resize(l layout) {
	if s.px.W != l.pxW() || s.px.H != l.pxH() {
		s.px.Resize(l.pxW(), l.pxH())
		s.skyValid = false
	}
}

// sameTheme compares the theme fields that affect the cached sky.
func sameTheme(a, b theme) bool {
	return a.skyTop == b.skyTop && a.skyMid == b.skyMid && a.skyBottom == b.skyBottom &&
		a.midAt == b.midAt && a.sun == b.sun && a.sunGlow == b.sunGlow &&
		a.sunX == b.sunX && a.sunY == b.sunY && a.sunR == b.sunR &&
		a.glow == b.glow && a.moon == b.moon && a.stars == b.stars && a.aurora == b.aurora
}

// drawSky renders the cached sky (gradient, glow, sun or moon, stars and
// aurora) and copies it into the frame.
func (s *scene) drawSky(l layout, th theme, t float64) {
	phase := 0
	if th.aurora > 0 {
		phase = int(t * 1.5) // the aurora drifts slowly, refreshed ~1.5 times a second
	}
	if !s.skyValid || s.skyW != s.px.W || s.skyH != s.px.H || !sameTheme(th, s.skyTheme) || phase != s.skyPhase {
		s.renderSky(l, th, phase)
		s.skyValid, s.skyW, s.skyH, s.skyTheme, s.skyPhase = true, s.px.W, s.px.H, th, phase
	}
	s.px.Copy(&s.sky)
	// A few stars twinkle on top of the cached sky.
	if th.stars > 0 {
		for _, st := range s.stars {
			if !st.twinkle {
				continue
			}
			v := math.Sin(t*2.1 + st.phase)
			if v > 0.55 {
				s.px.Blend(st.x, st.y, hex(0xffffff), th.stars)
			} else if v < -0.6 {
				s.px.Set(st.x, st.y, s.sky.At(st.x, st.y-1))
			}
		}
	}
}

func (s *scene) renderSky(l layout, th theme, phase int) {
	s.sky.Resize(s.px.W, s.px.H)
	skyPx := l.groundTop()
	aspect := float64(3 - l.ppc) // 1 for square half-block pixels, 2 for ASCII cells
	for y := 0; y < s.sky.H; y++ {
		// The gradient steps once per cell row, so plain sky cells are blank
		// spaces (one byte, no foreground) rather than two-colour half blocks.
		yc := y - y%l.ppc
		f := float64(yc) / float64(max(skyPx-1, 1))
		var c rgb
		if f < th.midAt {
			c = th.skyTop.Lerp(th.skyMid, f/th.midAt)
		} else {
			c = th.skyMid.Lerp(th.skyBottom, (f-th.midAt)/(1-th.midAt))
		}
		s.sky.FillRect(0, y, s.sky.W, y+1, c)
	}

	// Stars, generated per size so they stay put between frames.
	s.stars = s.stars[:0]
	if th.stars > 0 {
		rng := rand.New(rand.NewPCG(uint64(s.sky.W), uint64(skyPx)))
		n := s.sky.W * skyPx / 55
		for i := 0; i < n; i++ {
			st := star{x: rng.IntN(max(s.sky.W, 1)), y: rng.IntN(max(skyPx*2/3, 1)), bright: 0.35 + rng.Float64()*0.65, twinkle: i%6 == 0, phase: rng.Float64() * 6}
			s.stars = append(s.stars, st)
			s.sky.Blend(st.x, st.y, hex(0xfffbe8), st.bright*th.stars)
		}
	}

	// Aurora: soft translucent curtains near the top of the sky.
	if th.aurora > 0 {
		p := float64(phase) * 0.35
		for x := 0; x < s.sky.W; x++ {
			fx := float64(x) / math.Max(l.unitX, 1)
			yc := (0.2 + 0.06*math.Sin(fx*2.3+p) + 0.03*math.Sin(fx*5.1-p*0.7)) * float64(skyPx)
			th2 := (0.07 + 0.03*math.Sin(fx*3.7+p*1.3)) * float64(skyPx)
			for y := max(0, int(yc-th2*2)); y < min(skyPx, int(yc+th2*2)); y++ {
				d := math.Abs(float64(y)-yc) / th2
				a := math.Max(0, 1-d*d/4) * 0.45 * th.aurora
				col := hex(0x4dffb5).Lerp(hex(0x45b8ff), math.Max(0, math.Min(1, (float64(y)-yc)/th2*0.5+0.5)))
				s.sky.Blend(x, y, col, a)
			}
		}
	}

	// Sun or moon with a soft glow.
	cx := th.sunX * float64(s.sky.W)
	cy := th.sunY * float64(skyPx)
	r := th.sunR * float64(skyPx) * aspect
	if th.glow > 0 {
		gr := r * 3.2
		for y := max(0, int(cy-gr/aspect)); y < min(skyPx, int(cy+gr/aspect)+1); y++ {
			for x := max(0, int(cx-gr)); x < min(s.sky.W, int(cx+gr)+1); x++ {
				dx, dy := float64(x)+0.5-cx, (float64(y)+0.5-cy)*aspect
				d := math.Sqrt(dx*dx+dy*dy) / gr
				if d < 1 {
					s.sky.Blend(x, y, th.sunGlow, th.glow*(1-d)*(1-d))
				}
			}
		}
	}
	s.sky.Disc(cx, cy, r, aspect, th.sun, 0.8)
	if th.moon > 0 {
		crater := th.sun.Scale(0.86)
		s.sky.Disc(cx-r*0.3, cy-r*0.25/aspect, r*0.28, aspect, th.sun.Lerp(crater, th.moon), 0)
		s.sky.Disc(cx+r*0.35, cy+r*0.2/aspect, r*0.2, aspect, th.sun.Lerp(crater, th.moon), 0)
	}
}

// drawClouds draws the slow parallax cloud layer.
func (s *scene) drawClouds(l layout, th theme, scroll float64) {
	skyPx := float64(l.groundTop())
	aspect := float64(3 - l.ppc)
	off := scroll * 0.12
	tileCols := cloudTile * l.unitX
	for _, c := range s.clouds {
		// Position in columns, wrapped so the pattern tiles across the screen.
		x := math.Mod(c.x*l.unitX-off*l.unitX, tileCols)
		if x < 0 {
			x += tileCols
		}
		y := c.y * skyPx
		r := c.r * l.unitX
		for x0 := x - tileCols; x0 < float64(l.w)+r*3; x0 += tileCols {
			if x0 < -r*4 {
				continue
			}
			for p := 0; p < c.puffs; p++ {
				t := float64(p)/float64(max(c.puffs-1, 1)) - 0.5
				px := x0 + t*r*3.4
				pr := r * (1 - 0.45*math.Abs(t)*2*0.6)
				py := y - pr*0.35/aspect*(1-math.Abs(t))
				s.px.Disc(px, py, pr, aspect, th.cloud, 0)
			}
			// Flat, slightly shaded underside.
			yb := int(y + r*0.35/aspect)
			s.px.FillRect(int(x0-r*1.9), yb, int(x0+r*1.9), int(y+r*0.55/aspect)+1, th.cloudShade)
		}
	}
}

// drawHills draws two parallax hill layers resting on the ground. Heights
// are quantised to whole pixels and the layers scroll slowly, which keeps
// the number of cells that change per frame (and so SSH bandwidth) low.
func (s *scene) drawHills(l layout, th theme, scroll float64) {
	top := l.groundTop()
	u := l.unitY
	off := int(math.Floor(scroll * 0.15 * l.unitX))
	for x := 0; x < s.px.W; x++ {
		fx := float64(x+off) / l.unitX
		hf := (0.2 + 0.07*math.Sin(fx*1.7+1.3) + 0.045*math.Sin(fx*3.9+0.2)) * u
		y0 := top - int(math.Round(hf))
		s.px.FillColumn(x, y0, top, th.hillFar)
		s.px.Blend(x, y0, hex(0xffffff), 0.12)
	}
	// Near hills: rounded bushes made of plateaus, so neighbouring columns
	// usually share a height and a horizontal shift changes few cells.
	off = int(math.Floor(scroll * 0.32 * l.unitX))
	step := max(2, int(math.Round(0.018*u)))
	for x := 0; x < s.px.W; x++ {
		fx := float64(x+off) / l.unitX
		hn := (0.075 + 0.045*math.Abs(math.Sin(fx*4.6))) * u
		h := int(math.Round(hn/float64(step))) * step
		y0 := top - h
		mid := top - h*9/20
		s.px.FillColumn(x, y0, mid, th.hillNear)
		s.px.FillColumn(x, mid, top, th.hillNS)
		s.px.Blend(x, y0, hex(0xffffff), 0.18)
	}
}

// drawGround draws the scrolling grass and the dirt below it. Only the grass
// stripes move; the dirt is static and shaded per cell row so it costs no
// bandwidth after the first frame.
func (s *scene) drawGround(l layout, th theme, scroll float64) {
	top := l.groundTop()
	shift := int(math.Floor(scroll * l.unitX))
	grass := l.ppc // one cell row of grass
	stripe := 6
	for x := 0; x < s.px.W; x++ {
		c := th.grassLight
		if ((x+shift)/stripe)%2 == 0 {
			c = th.grassDark
		}
		s.px.FillColumn(x, top, top+grass, c)
	}
	s.px.FillRect(0, top+grass, s.px.W, top+grass+1, th.grassEdge)
	rows := s.px.H - top - grass - 1
	for y := top + grass + 1; y < s.px.H; y++ {
		cell := (y - top) / l.ppc
		f := float64(cell) / float64(max(rows/l.ppc, 1))
		d := th.dirt.Lerp(th.dirtDark, f*0.7)
		s.px.FillRect(0, y, s.px.W, y+1, d)
		// Fixed specks give the dirt some texture.
		if (y-top)%l.ppc == 0 {
			for x := (cell * 7) % 13; x < s.px.W; x += 13 + cell%5 {
				s.px.Set(x, y, th.dirtDark.Scale(0.92))
				if l.ppc == 2 {
					s.px.Set(x, y+1, th.dirtDark.Scale(0.92))
				}
			}
		}
	}
}

// pipeBodyColor shades a pipe body from left to right: highlight, base and
// a dark right edge. Three bands (four colour changes per row, including the
// sky on either side) keep the cylinder look while limiting how much of each
// row changes as the pipe scrolls, which is most of the game's bandwidth.
func pipeBodyColor(t, light float64) rgb {
	var c rgb
	switch {
	case t < 0.3:
		c = colPipeLight
	case t < 0.78:
		c = colPipeBase
	default:
		c = colPipeShade.Lerp(colPipeEdge, 0.5)
	}
	return c.Scale(light)
}

// pipeCapColor shades the wider cap, which also gets a dark outline.
func pipeCapColor(t, light float64) rgb {
	var c rgb
	switch {
	case t < 0.12:
		c = colPipeEdge
	case t < 0.36:
		c = colPipeLight
	case t < 0.7:
		c = colPipeBase
	case t < 0.88:
		c = colPipeShade
	default:
		c = colPipeEdge
	}
	return c.Scale(light)
}

func (s *scene) drawPipes(l layout, th theme, w *game.World) {
	p := w.Params()
	cam := w.Camera()
	top := l.groundTop()
	capH := max(2, int(math.Round(0.05*l.unitY)))
	over := max(1, int(math.Round(0.018*l.unitX)))
	clipL, clipR := l.playX, l.playX+l.playW
	for _, pp := range w.Pipes() {
		x0 := int(math.Round(l.worldX(pp.X - cam)))
		x1 := int(math.Round(l.worldX(pp.X + p.PipeWidth - cam)))
		if x1 < clipL-over || x0 > clipR+over {
			continue
		}
		gt := int(math.Round(l.worldY(pp.GapTop())))
		gb := int(math.Round(l.worldY(pp.GapBottom())))
		bw := max(x1-x0, 1)
		for x := max(x0, clipL); x < min(x1, clipR); x++ {
			c := pipeBodyColor((float64(x-x0)+0.5)/float64(bw), th.pipeLight)
			s.px.FillColumn(x, 0, gt-capH, c)
			s.px.FillColumn(x, gb+capH, top, c)
		}
		cx0, cx1 := x0-over, x1+over
		cw := max(cx1-cx0, 1)
		for x := max(cx0, clipL); x < min(cx1, clipR); x++ {
			c := pipeCapColor((float64(x-cx0)+0.5)/float64(cw), th.pipeLight*1.05)
			s.px.FillColumn(x, gt-capH, gt, c)
			s.px.FillColumn(x, gb, gb+capH, c)
			edge := colPipeEdge.Scale(th.pipeLight)
			s.px.Set(x, gt-1, edge)
			s.px.Set(x, gb, edge)
			if capH >= 3 {
				s.px.Set(x, gt-capH, edge)
				s.px.Set(x, gb+capH-1, edge)
			}
		}
	}
}

// birdScale returns the sprite scale (columns and pixel rows per bird unit)
// matching the collision box at the current layout. The sprite is drawn a
// little larger than the hitbox so near misses look fair.
func birdScale(l layout, p game.Params) (sx, sy float64) {
	sx = math.Max(2.8, 1.3*p.BirdWidth*l.unitX/1.6)
	sy = sx / float64(3-l.ppc)
	return sx, sy
}

// drawBird draws the bird at its world position.
func (s *scene) drawBird(l layout, p game.Params, y, wing, angle float64, skinIdx int, tint rgb, tintAmt float64) {
	sx, sy := birdScale(l, p)
	sp := s.sprites.get(sx, sy, wing, angle, skinIdx)
	sp.blit(&s.px, l.worldX(p.BirdX), l.worldY(y), tint, tintAmt)
}

// drawBirdAt draws a bird at an arbitrary pixel position and scale.
func (s *scene) drawBirdAt(cx, cy, sx, sy, wing, angle float64, skinIdx int) {
	s.sprites.get(sx, sy, wing, angle, skinIdx).blit(&s.px, cx, cy, rgb{}, 0)
}

// particle is a short-lived pixel effect.
type particle struct {
	x, y, vx, vy float64
	life, ttl    float64
	col          rgb
	gravity      float64
}

const maxParticles = 96

type particles struct{ list []particle }

func (ps *particles) add(p particle) {
	if len(ps.list) >= maxParticles {
		copy(ps.list, ps.list[1:])
		ps.list = ps.list[:len(ps.list)-1]
	}
	ps.list = append(ps.list, p)
}

func (ps *particles) update(dt float64) {
	n := 0
	for _, p := range ps.list {
		p.life += dt
		if p.life >= p.ttl {
			continue
		}
		p.vy += p.gravity * dt
		p.x += p.vx * dt
		p.y += p.vy * dt
		ps.list[n] = p
		n++
	}
	ps.list = ps.list[:n]
}

func (ps *particles) draw(px *canvas.Pixels) {
	for _, p := range ps.list {
		a := 1 - p.life/p.ttl
		px.Blend(int(p.x), int(p.y), p.col, math.Min(1, a*1.5))
	}
}
