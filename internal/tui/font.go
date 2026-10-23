package tui

import "flap/internal/canvas"

// pixelFont is a bitmap font drawn into a pixel layer.
type pixelFont struct {
	w, h    int
	spacing int
	glyphs  map[rune][]string
}

// digits3x5 is a compact font for the in-game score.
var digits3x5 = pixelFont{w: 3, h: 5, spacing: 1, glyphs: map[rune][]string{
	'0': {"###", "#.#", "#.#", "#.#", "###"},
	'1': {".#.", "##.", ".#.", ".#.", "###"},
	'2': {"###", "..#", "###", "#..", "###"},
	'3': {"###", "..#", ".##", "..#", "###"},
	'4': {"#.#", "#.#", "###", "..#", "..#"},
	'5': {"###", "#..", "###", "..#", "###"},
	'6': {"###", "#..", "###", "#.#", "###"},
	'7': {"###", "..#", ".#.", ".#.", ".#."},
	'8': {"###", "#.#", "###", "#.#", "###"},
	'9': {"###", "#.#", "###", "..#", "###"},
}}

// font5x7 is an original chunky display font for titles and banners.
var font5x7 = pixelFont{w: 5, h: 7, spacing: 1, glyphs: map[rune][]string{
	'A': {".###.", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'B': {"####.", "#...#", "#...#", "####.", "#...#", "#...#", "####."},
	'C': {".####", "#....", "#....", "#....", "#....", "#....", ".####"},
	'D': {"####.", "#...#", "#...#", "#...#", "#...#", "#...#", "####."},
	'E': {"#####", "#....", "#....", "####.", "#....", "#....", "#####"},
	'F': {"#####", "#....", "#....", "####.", "#....", "#....", "#...."},
	'G': {".####", "#....", "#....", "#..##", "#...#", "#...#", ".###."},
	'H': {"#...#", "#...#", "#...#", "#####", "#...#", "#...#", "#...#"},
	'I': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "#####"},
	'J': {"..###", "...#.", "...#.", "...#.", "#..#.", "#..#.", ".##.."},
	'K': {"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"},
	'L': {"#....", "#....", "#....", "#....", "#....", "#....", "#####"},
	'M': {"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"},
	'N': {"#...#", "##..#", "#.#.#", "#..##", "#...#", "#...#", "#...#"},
	'O': {".###.", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'P': {"####.", "#...#", "#...#", "####.", "#....", "#....", "#...."},
	'Q': {".###.", "#...#", "#...#", "#...#", "#.#.#", "#..#.", ".##.#"},
	'R': {"####.", "#...#", "#...#", "####.", "#.#..", "#..#.", "#...#"},
	'S': {".####", "#....", "#....", ".###.", "....#", "....#", "####."},
	'T': {"#####", "..#..", "..#..", "..#..", "..#..", "..#..", "..#.."},
	'U': {"#...#", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
	'V': {"#...#", "#...#", "#...#", "#...#", "#...#", ".#.#.", "..#.."},
	'W': {"#...#", "#...#", "#...#", "#.#.#", "#.#.#", "##.##", "#...#"},
	'X': {"#...#", "#...#", ".#.#.", "..#..", ".#.#.", "#...#", "#...#"},
	'Y': {"#...#", "#...#", ".#.#.", "..#..", "..#..", "..#..", "..#.."},
	'Z': {"#####", "....#", "...#.", "..#..", ".#...", "#....", "#####"},
	'0': {".###.", "#...#", "#..##", "#.#.#", "##..#", "#...#", ".###."},
	'1': {"..#..", ".##..", "..#..", "..#..", "..#..", "..#..", ".###."},
	'2': {".###.", "#...#", "....#", "...#.", "..#..", ".#...", "#####"},
	'3': {"####.", "....#", "....#", ".###.", "....#", "....#", "####."},
	'4': {"...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."},
	'5': {"#####", "#....", "####.", "....#", "....#", "#...#", ".###."},
	'6': {".###.", "#....", "#....", "####.", "#...#", "#...#", ".###."},
	'7': {"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."},
	'8': {".###.", "#...#", "#...#", ".###.", "#...#", "#...#", ".###."},
	'9': {".###.", "#...#", "#...#", ".####", "....#", "....#", ".###."},
	'!': {"..#..", "..#..", "..#..", "..#..", "..#..", ".....", "..#.."},
	'?': {".###.", "#...#", "....#", "...#.", "..#..", ".....", "..#.."},
	'.': {".....", ".....", ".....", ".....", ".....", ".....", "..#.."},
	'-': {".....", ".....", ".....", ".###.", ".....", ".....", "....."},
	' ': {".....", ".....", ".....", ".....", ".....", ".....", "....."},
}}

// textWidth returns the pixel width of s at horizontal scale sx.
func (f *pixelFont) textWidth(s string, sx int) int {
	n := 0
	for range s {
		n++
	}
	if n == 0 {
		return 0
	}
	return (n*(f.w+f.spacing) - f.spacing) * sx
}

// height returns the glyph height at vertical scale sy.
func (f *pixelFont) height(sy int) int { return f.h * sy }

// draw renders s into px with its top-left corner at (x, y), scaling glyph
// pixels by sx horizontally and sy vertically. fill picks the colour of each
// lit pixel from its row within the glyph (0..h*sy-1), so text can carry a
// vertical gradient. outline, when non-nil, draws a 1-pixel border around
// the glyphs; shadow, when non-nil, a drop shadow.
func (f *pixelFont) draw(px *canvas.Pixels, x, y int, s string, sx, sy int, fill func(row int) rgb, outline, shadow *rgb) {
	lit := func(gx, gy int, g []string) bool {
		if gx < 0 || gy < 0 || gy >= len(g) || gx >= len(g[gy]) {
			return false
		}
		return g[gy][gx] == '#'
	}
	pass := func(mode int) {
		cx := x
		for _, r := range s {
			g, ok := f.glyphs[r]
			if !ok {
				g = f.glyphs['?']
			}
			for gy := 0; gy < f.h; gy++ {
				for gx := 0; gx < f.w; gx++ {
					if !lit(gx, gy, g) {
						continue
					}
					for oy := 0; oy < sy; oy++ {
						for ox := 0; ox < sx; ox++ {
							X, Y := cx+gx*sx+ox, y+gy*sy+oy
							switch mode {
							case 0:
								px.Set(X+max(1, sx/2), Y+max(1, sy/2), *shadow)
							case 1:
								for dy := -1; dy <= 1; dy++ {
									for dx := -1; dx <= 1; dx++ {
										px.Set(X+dx, Y+dy, *outline)
									}
								}
							case 2:
								px.Set(X, Y, fill(gy*sy+oy))
							}
						}
					}
				}
			}
			cx += (f.w + f.spacing) * sx
		}
	}
	if shadow != nil {
		pass(0)
	}
	if outline != nil {
		pass(1)
	}
	pass(2)
}

// solid returns a fill function of a single colour.
func solid(c rgb) func(int) rgb { return func(int) rgb { return c } }

// gradient returns a fill function that blends top to bottom over h rows.
func gradient(top, bottom rgb, h int) func(int) rgb {
	return func(row int) rgb {
		if h <= 1 {
			return top
		}
		return top.Lerp(bottom, float64(row)/float64(h-1))
	}
}
