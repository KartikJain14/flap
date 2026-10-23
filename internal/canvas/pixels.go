package canvas

import "math"

// upperHalf is the glyph used to show two vertically stacked pixels in one
// cell: the foreground colours the top pixel and the background the bottom.
const upperHalf = '▀'

// Pixels is an off-screen bitmap. In half-block mode every terminal cell
// shows two pixels stacked vertically, which makes pixels roughly square.
type Pixels struct {
	W, H int
	Pix  []RGB
}

// Resize changes the bitmap size, reusing memory where possible.
func (p *Pixels) Resize(w, h int) {
	w, h = max(w, 0), max(h, 0)
	p.W, p.H = w, h
	if n := w * h; cap(p.Pix) >= n {
		p.Pix = p.Pix[:n]
	} else {
		p.Pix = make([]RGB, n)
	}
}

// In reports whether (x, y) lies inside the bitmap.
func (p *Pixels) In(x, y int) bool { return x >= 0 && y >= 0 && x < p.W && y < p.H }

// At returns the pixel at (x, y), or black when out of bounds.
func (p *Pixels) At(x, y int) RGB {
	if !p.In(x, y) {
		return RGB{}
	}
	return p.Pix[y*p.W+x]
}

// Set paints one pixel, ignoring out-of-bounds coordinates.
func (p *Pixels) Set(x, y int, c RGB) {
	if p.In(x, y) {
		p.Pix[y*p.W+x] = c
	}
}

// Blend mixes c into the pixel at (x, y) with opacity a.
func (p *Pixels) Blend(x, y int, c RGB, a float64) {
	if p.In(x, y) {
		i := y*p.W + x
		p.Pix[i] = p.Pix[i].Lerp(c, a)
	}
}

// FillRect paints the half-open pixel rectangle [x0,x1)×[y0,y1).
func (p *Pixels) FillRect(x0, y0, x1, y1 int, c RGB) {
	x0, y0 = max(x0, 0), max(y0, 0)
	x1, y1 = min(x1, p.W), min(y1, p.H)
	for y := y0; y < y1; y++ {
		row := p.Pix[y*p.W : y*p.W+p.W]
		for x := x0; x < x1; x++ {
			row[x] = c
		}
	}
}

// FillColumn paints the half-open vertical span [y0,y1) of column x.
func (p *Pixels) FillColumn(x, y0, y1 int, c RGB) {
	if x < 0 || x >= p.W {
		return
	}
	y0, y1 = max(y0, 0), min(y1, p.H)
	for y := y0; y < y1; y++ {
		p.Pix[y*p.W+x] = c
	}
}

// Disc paints a filled circle with an optional soft edge of width feather
// pixels. ry/rx corrects for non-square pixels.
func (p *Pixels) Disc(cx, cy, r, aspect float64, c RGB, feather float64) {
	x0, x1 := int(math.Floor(cx-r-feather)), int(math.Ceil(cx+r+feather))
	ry := r / aspect
	y0, y1 := int(math.Floor(cy-ry-feather)), int(math.Ceil(cy+ry+feather))
	for y := max(y0, 0); y <= min(y1, p.H-1); y++ {
		for x := max(x0, 0); x <= min(x1, p.W-1); x++ {
			dx := float64(x) + 0.5 - cx
			dy := (float64(y) + 0.5 - cy) * aspect
			d := math.Sqrt(dx*dx+dy*dy) - r
			switch {
			case d <= 0:
				p.Pix[y*p.W+x] = c
			case feather > 0 && d < feather:
				p.Blend(x, y, c, 1-d/feather)
			}
		}
	}
}

// Copy copies src into p. Both must have the same size.
func (p *Pixels) Copy(src *Pixels) {
	copy(p.Pix, src.Pix)
}

// DrawPixels composites a bitmap onto the canvas at cell (ox, oy). With
// rowsPerCell == 2 each cell shows two pixels using a half block; with 1 each
// pixel becomes a coloured space, which only needs ASCII.
func (c *Canvas) DrawPixels(p *Pixels, ox, oy, rowsPerCell int) {
	rows := (p.H + rowsPerCell - 1) / rowsPerCell
	for cy := 0; cy < rows; cy++ {
		y := oy + cy
		if y < 0 || y >= c.h {
			continue
		}
		for cx := 0; cx < p.W; cx++ {
			x := ox + cx
			if x < 0 || x >= c.w {
				continue
			}
			top := p.Pix[cy*rowsPerCell*p.W+cx]
			cell := &c.cells[y*c.w+x]
			if rowsPerCell == 1 {
				*cell = Cell{Ch: ' ', Fg: top, Bg: top}
				continue
			}
			bottom := top
			if yb := cy*2 + 1; yb < p.H {
				bottom = p.Pix[yb*p.W+cx]
			}
			if top == bottom {
				*cell = Cell{Ch: ' ', Fg: top, Bg: top}
			} else {
				*cell = Cell{Ch: upperHalf, Fg: top, Bg: bottom}
			}
		}
	}
}
