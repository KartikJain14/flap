package canvas

import (
	"fmt"
	"html"
	"io"
	"strings"
)

// WriteSVG renders the canvas as an SVG image, approximating how a terminal
// in the given colour mode would display it. It is used for screenshots in
// the documentation and for visual review during development.
func (c *Canvas) WriteSVG(w io.Writer, mode ColorMode) error {
	const cw, ch = 9, 18
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges">`+"\n",
		c.w*cw, c.h*ch, c.w*cw, c.h*ch)
	b.WriteString(`<style>text{font-family:"JetBrains Mono","DejaVu Sans Mono",Menlo,Consolas,monospace;font-size:15px;white-space:pre}</style>` + "\n")
	q := func(col RGB) string {
		v := Quantize(col, mode)
		return fmt.Sprintf("#%02x%02x%02x", v.R, v.G, v.B)
	}
	for y := 0; y < c.h; y++ {
		row := c.cells[y*c.w : (y+1)*c.w]
		// Backgrounds (bottom halves), merged into horizontal runs.
		for x := 0; x < c.w; {
			bg := q(row[x].Bg)
			x0 := x
			for x < c.w && q(row[x].Bg) == bg {
				x++
			}
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, x0*cw, y*ch, (x-x0)*cw, ch, bg)
		}
		// Top halves of half-block cells.
		for x := 0; x < c.w; {
			if row[x].Ch != upperHalf {
				x++
				continue
			}
			fg := q(row[x].Fg)
			x0 := x
			for x < c.w && row[x].Ch == upperHalf && q(row[x].Fg) == fg {
				x++
			}
			fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, x0*cw, y*ch, (x-x0)*cw, ch/2, fg)
		}
		b.WriteByte('\n')
		// Glyphs.
		for x := 0; x < c.w; x++ {
			cell := row[x]
			if cell.Ch == ' ' || cell.Ch == 0 || cell.Ch == upperHalf {
				continue
			}
			r := cell.Ch
			if !SafeRune(r) {
				r = '?'
			}
			weight := ""
			if cell.Attr&Bold != 0 {
				weight = ` font-weight="bold"`
			}
			fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s"%s>%s</text>`, x*cw, y*ch+14, q(cell.Fg), weight, html.EscapeString(string(r)))
		}
		b.WriteByte('\n')
	}
	b.WriteString("</svg>\n")
	_, err := io.WriteString(w, b.String())
	return err
}
