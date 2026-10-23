package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"flap/internal/canvas"
	"flap/internal/game"
	"flap/internal/store"
)

// glyphSet holds the symbols used by the UI in each glyph mode.
type glyphSet struct {
	box                                canvas.BoxStyle
	play, bullet, star, check, cross   string
	warn, dot, ell, left, right, pause string
	cursor, sep, hline, you            string
}

func glyphsFor(m GlyphMode) glyphSet {
	if m == ASCII {
		return glyphSet{box: canvas.ASCIIBox, play: ">", bullet: "*", star: "*", check: "+", cross: "x",
			warn: "!", dot: "o", ell: "...", left: "<", right: ">", pause: "||", cursor: "_", sep: "|", hline: "-", you: ">"}
	}
	return glyphSet{box: canvas.RoundBox, play: "▶", bullet: "•", star: "★", check: "✓", cross: "✗",
		warn: "!", dot: "●", ell: "…", left: "←", right: "→", pause: "❚❚", cursor: "█", sep: "·", hline: "─", you: "▶"}
}

func (a *App) trunc(s string, w int) string {
	if canvas.StringWidth(s) <= w {
		return s
	}
	ew := canvas.StringWidth(a.g.ell)
	if w <= ew {
		return strings.Repeat(".", max(w, 0))
	}
	out := canvas.Truncate(s, w-ew+1)
	return strings.TrimSuffix(out, "…") + a.g.ell
}

func (a *App) render() {
	a.cv.Resize(a.w, a.h)
	if a.lay.tooSmall {
		a.drawTooSmall()
	} else {
		a.drawScene()
		switch a.screen {
		case screenWelcome:
			a.drawWelcome()
		case screenPlay:
			a.drawHUD()
			if a.paused && a.overlay == overlayNone {
				a.drawPause()
			}
		case screenOver:
			a.drawHUD()
			a.drawGameOver()
		case screenBoard:
			a.cv.Shade(a.cv.Bounds(), 0.42)
			a.drawBoard()
		}
		switch a.overlay {
		case overlayHelp:
			a.cv.Shade(a.cv.Bounds(), 0.5)
			a.drawHelp()
		case overlayName:
			a.cv.Shade(a.cv.Bounds(), 0.5)
			a.drawName()
		}
		if a.notice != "" {
			a.drawNotice()
		}
	}
	a.buf.Reset()
	a.cv.Encode(&a.buf, a.color)
	a.cache = a.buf.String()
}

// worldVisible reports whether the current run should be drawn.
func (a *App) worldVisible() bool {
	if a.world == nil {
		return false
	}
	switch a.screen {
	case screenPlay, screenOver:
		return true
	case screenBoard:
		return a.prevScreen == screenOver
	}
	return false
}

func (a *App) drawScene() {
	l := a.lay
	th := a.currentTheme()
	sc := a.scene
	sc.resize(l)
	scroll := a.menuScroll
	if a.worldVisible() {
		scroll = a.world.Camera()
	}
	sc.drawSky(l, th, a.t)
	sc.drawClouds(l, th, scroll)
	sc.drawHills(l, th, scroll)
	var wl welcomeLayout
	if a.worldVisible() {
		sc.drawPipes(l, th, a.world)
	} else if a.screen == screenWelcome || a.screen == screenBoard {
		wl = a.welcomeLayout()
	}
	sc.drawGround(l, th, scroll)

	if a.worldVisible() {
		w := a.world
		b := w.Bird()
		wing := 0.0
		switch {
		case w.Phase() != game.Playing:
			wing = -1
		case b.SinceFlap < 0.32:
			wing = []float64{1, 0, -1, 0}[int(b.SinceFlap/0.08)%4]
		case b.VY > a.opt.Params.MaxFallSpeed*0.7:
			wing = -1
		}
		tintAmt := 0.0
		if w.Phase() != game.Playing && int(a.t*10)%2 == 0 && a.t-a.overAt < 0.6 {
			tintAmt = 0.45
		}
		sc.drawBird(l, a.opt.Params, b.Y, wing, a.birdAngle, a.skinIndex(), hex(0xffffff), tintAmt)
	} else if a.screen == screenWelcome {
		a.drawWelcomePixels(wl)
	}
	a.fx.draw(&sc.px)
	if a.screen == screenPlay || a.screen == screenOver {
		a.drawScorePixels()
	}
	if a.screen == screenPlay && a.countdown > 0 && !a.paused {
		n := int(math.Ceil(a.countdown / 0.5))
		s := strconv.Itoa(n)
		fsx, fsy := a.fontScale(max(1, int(float64(l.playRows)/14)))
		fw, fh := font5x7.textWidth(s, fsx), font5x7.height(fsy)
		x := l.playX + (l.playW-fw)/2
		y := l.playTop*l.ppc + (l.playRows*l.ppc-fh)/2
		out, sh := colScoreOutline, hex(0x000000)
		font5x7.draw(&sc.px, x, y, s, fsx, fsy, solid(colAccent), &out, &sh)
	}
	a.cv.DrawPixels(&sc.px, 0, 0, l.ppc)
	if l.letterboxed() {
		a.cv.Shade(canvas.Rect{X: 0, Y: 0, W: l.playX, H: l.h}, 0.55)
		a.cv.Shade(canvas.Rect{X: l.playX + l.playW, Y: 0, W: l.w - l.playX - l.playW, H: l.h}, 0.55)
	}
	if l.playTop > 0 && a.worldVisible() {
		// Above the ceiling of a shortened play area.
		a.cv.Shade(canvas.Rect{X: 0, Y: 0, W: l.w, H: l.playTop}, 0.8)
	}
}

// fontScale converts a pixel-font scale into horizontal and vertical pixel
// factors. ASCII cells are one pixel tall but half as wide, so glyphs are
// doubled horizontally to keep their proportions.
func (a *App) fontScale(scale int) (sx, sy int) {
	return scale * (3 - a.lay.ppc), scale
}

func (a *App) scoreScale() int {
	switch u := a.lay.unitY; {
	case u < 46:
		return 1
	case u < 96:
		return 2
	}
	return 3
}

func (a *App) drawScorePixels() {
	l := a.lay
	s := strconv.Itoa(a.world.Score())
	sx, sy := a.fontScale(a.scoreScale())
	fw := digits3x5.textWidth(s, sx)
	x := l.playX + (l.playW-fw)/2
	y := l.playTop*l.ppc + 1 + l.ppc
	fill := colScore
	if a.runBest {
		fill = colGold
	}
	fill = fill.Lerp(colGold, a.scoreFlash)
	out := colScoreOutline
	digits3x5.draw(&a.scene.px, x, y, s, sx, sy, solid(fill), &out, nil)
}

// drawHUD draws the text parts of the in-game heads-up display.
func (a *App) drawHUD() {
	l := a.lay
	row := l.playTop
	right := l.playX + l.playW - 1
	best := a.Best()
	if a.world != nil {
		best = max(best, a.world.Score())
	}
	label := "BEST "
	val := strconv.Itoa(best)
	x := right - canvas.StringWidth(label+val)
	a.cv.Text(x, row, label, colText.Scale(0.85), 0)
	a.cv.Text(x+len(label), row, val, colAccent, canvas.Bold)
	if a.world != nil {
		th := themeFor(a.world.Stage())
		a.cv.Text(l.playX+1, row, strings.ToUpper(th.name), colText.Scale(0.8), 0)
	}
	if a.bannerT > 0 && a.screen == screenPlay {
		fade := math.Min(1, math.Min((2.2-a.bannerT)/0.25, a.bannerT/0.5))
		y := l.playTop + max(3, l.playRows/4)
		title := a.g.star + " " + a.banner + " " + a.g.star
		a.cv.TextCentered(l.playX, y, l.playW, title, colText.Lerp(colAccent, fade), canvas.Bold)
		a.cv.TextCentered(l.playX, y+1, l.playW, a.bannerSub, colText.Scale(0.5+0.4*fade), 0)
	}
	if a.screen == screenPlay && a.runs == 0 && a.world.Duration() < 4 && a.world.Phase() == game.Playing {
		hint := "SPACE flap " + a.g.sep + " P pause " + a.g.sep + " Q quit"
		a.cv.TextCentered(l.playX, l.skyRows-2, l.playW, a.trunc(hint, l.playW-2), colText, canvas.Bold)
	}
}

// card draws a framed panel with a drop shadow and an optional title.
func (a *App) card(r canvas.Rect, title string, border rgb) {
	a.cv.Shade(canvas.Rect{X: r.X + 1, Y: r.Y + 1, W: r.W, H: r.H}, 0.5)
	a.cv.Fill(r, colCard)
	bg := colCard
	a.cv.Box(r, a.g.box, border, &bg)
	if title != "" {
		t := " " + a.trunc(title, r.W-6) + " "
		a.cv.TextCentered(r.X, r.Y, r.W, t, colCardTitle, canvas.Bold)
	}
}

type keyHint struct {
	key, label string
	off        bool // drawn dimmed when the action is not available yet
}

// hints draws key caps and labels centred in [x, x+w), dropping hints from
// the end until they fit.
func (a *App) hints(x, y, w int, hs []keyHint) {
	for n := len(hs); n > 0; n-- {
		total := 0
		for i, h := range hs[:n] {
			if i > 0 {
				total += 2
			}
			total += canvas.StringWidth(h.key) + 2 + 1 + canvas.StringWidth(h.label)
		}
		if total > w && n > 1 {
			continue
		}
		cx := x + max(0, (w-total)/2)
		for i, h := range hs[:n] {
			if i > 0 {
				cx += 2
			}
			kbg, lc := colKeyBg, colMuted
			if h.off {
				kbg, lc = colFaint, colFaint
			}
			cx += a.cv.TextBg(cx, y, " "+h.key+" ", colKeyFg, kbg, canvas.Bold)
			cx += a.cv.Text(cx, y, " "+h.label, lc, 0)
		}
		return
	}
}

type welcomeLayout struct {
	titleScale     int
	titleX, titleY int // pixels
	birdX, birdY   float64
	birdSX, birdSY float64
	subRow         int
	showSub        bool
	card           canvas.Rect
	boardN         int
	roomy          bool
}

func (a *App) welcomeCardLines(boardN int, roomy bool) int {
	n := 3 // prompt, player, hints
	if a.sess.Guest || a.sess.StorageDown {
		n++
	}
	if roomy {
		n += 2 // after the prompt and before the hints
	}
	if boardN > 0 {
		n += 2 + boardN // gap, header, entries
	}
	return n
}

// welcomeLayout picks the richest welcome screen that fits: title scale,
// leaderboard preview size and spacing all shrink on small terminals.
func (a *App) welcomeLayout() welcomeLayout {
	l := a.lay
	avail := l.skyRows - 1
	cardW := min(l.w-4, 62)
	for _, scale := range []int{3, 2, 1} {
		sx, sy := a.fontScale(scale)
		titleW := font5x7.textWidth("FLAP", sx) + 2
		titleH := font5x7.height(sy) + 3
		titleRows := (titleH + l.ppc - 1) / l.ppc
		birdSY := float64(font5x7.height(sy)) * 0.5
		birdSX := birdSY * float64(3-l.ppc)
		birdW := int(math.Ceil(2.3 * birdSX))
		groupW := birdW + 2 + titleW
		if groupW > l.w-4 || (scale > 1 && (titleRows > avail*3/10 || groupW > l.w*7/10)) {
			continue
		}
		// The preview shows up to five entries but never reserves rows for
		// entries that do not exist.
		want := min(5, max(1, len(a.top)))
		for _, boardN := range []int{want, min(want, 3), 0} {
			for _, roomy := range []bool{true, false} {
				for _, sub := range []bool{true, false} {
					cardH := a.welcomeCardLines(boardN, roomy) + 2
					total := titleRows + 1 + cardH
					if sub {
						total++
					}
					if total > avail {
						continue
					}
					top := 1 + (avail-total)/2
					groupX := (l.w - groupW) / 2
					wl := welcomeLayout{titleScale: scale, boardN: boardN, roomy: roomy, showSub: sub, birdSX: birdSX, birdSY: birdSY}
					wl.titleX = groupX + birdW + 2
					wl.titleY = top * l.ppc
					wl.birdX = float64(groupX) + float64(birdW)/2
					wl.birdY = float64(wl.titleY) + float64(titleH)/2 + math.Sin(a.t*2.4)*float64(l.ppc)
					row := top + titleRows
					if sub {
						wl.subRow = row
						row++
					}
					wl.card = canvas.Rect{X: (l.w - cardW) / 2, Y: row + 1, W: cardW, H: cardH}
					return wl
				}
			}
		}
	}
	// Last resort for very short terminals: just the essentials.
	cardH := a.welcomeCardLines(0, false) + 2
	return welcomeLayout{titleScale: 0, card: canvas.Rect{X: (l.w - cardW) / 2, Y: max(1, (l.skyRows-cardH)/2), W: cardW, H: cardH}}
}

func (a *App) drawWelcomePixels(wl welcomeLayout) {
	if wl.titleScale == 0 {
		return
	}
	px := &a.scene.px
	out, sh := hex(0x3a1f0c), hex(0x1d2b4f).Scale(0.9)
	sx, sy := a.fontScale(wl.titleScale)
	fh := font5x7.height(sy)
	font5x7.draw(px, wl.titleX+1, wl.titleY+1, "FLAP", sx, sy, gradient(hex(0xfff07a), hex(0xff8a2a), fh), &out, &sh)
	wing := []float64{1, 0, -1, 0}[int(a.t/0.11)%4]
	a.scene.drawBirdAt(wl.birdX, wl.birdY, wl.birdSX, wl.birdSY, wing, -8+6*math.Sin(a.t*2.4), a.skinIndex())
}

func (a *App) drawWelcome() {
	l := a.lay
	wl := a.welcomeLayout()
	if wl.titleScale == 0 && wl.card.Y > 0 {
		a.cv.TextCentered(0, wl.card.Y-1, l.w, "F L A P", colAccent, canvas.Bold)
	}
	if wl.showSub {
		a.cv.TextCentered(0, wl.subRow, l.w, a.trunc("a tiny arcade you play over SSH", l.w-2), hex(0x14213f), canvas.Bold)
	}
	r := wl.card
	a.card(r, "", colCardBorder)
	in := canvas.Rect{X: r.X + 2, Y: r.Y + 1, W: r.W - 4, H: r.H - 2}
	y := in.Y

	// Blinking prompt.
	pc := colAccent
	if int(a.t/0.6)%2 == 1 {
		pc = colAccent.Lerp(colCard, 0.45)
	}
	prompt := a.g.play + "  PRESS  SPACE  TO PLAY"
	a.cv.TextCentered(in.X, y, in.W, a.trunc(prompt, in.W), pc, canvas.Bold)
	y++
	if wl.roomy {
		y++
	}

	// Player line.
	name := a.sess.Player.Name
	if name == "" {
		name = "guest"
	}
	right := "BEST " + strconv.Itoa(a.Best())
	if a.rank > 0 && in.W >= 44 {
		right += fmt.Sprintf("  #%d of %d", a.rank, a.ranked)
	}
	nx := in.X
	label := "Playing as "
	if in.W < 40 {
		label = ""
	}
	nx += a.cv.Text(nx, y, label, colMuted, 0)
	maxName := in.W - canvas.StringWidth(right) - canvas.StringWidth(label) - 2
	if a.sess.Guest {
		maxName -= 8
	}
	nx += a.cv.Text(nx, y, a.trunc(name, max(maxName, 4)), skins[a.skinIndex()].body, canvas.Bold)
	if a.sess.Guest {
		a.cv.TextBg(nx+1, y, " GUEST ", colKeyFg, colWarn, canvas.Bold)
	}
	a.cv.TextRight(in.X+in.W, y, right, colText, canvas.Bold)
	y++
	switch {
	case a.sess.StorageDown:
		a.cv.Text(in.X, y, a.trunc(a.g.warn+" Scores can't be saved right now; you can still play.", in.W), colWarn, 0)
		y++
	case a.sess.Guest:
		a.cv.Text(in.X, y, a.trunc("No SSH key offered: scores stay with this guest name.", in.W), colFaint, 0)
		y++
	}

	if wl.boardN > 0 {
		y++
		a.cv.Text(in.X, y, "TOP PILOTS", colMuted, canvas.Bold)
		if a.topErr {
			a.cv.TextRight(in.X+in.W, y, "leaderboard unavailable", colWarn, 0)
		}
		y++
		for i := 0; i < wl.boardN; i++ {
			if i < len(a.top) {
				a.boardRow(in.X, y, in.W, a.top[i], false)
			} else if i == 0 && len(a.top) == 0 && !a.topLoading && !a.topErr {
				a.cv.Text(in.X, y, "No scores yet. Be the first!", colFaint, 0)
			}
			y++
		}
	}

	hs := []keyHint{{key: "SPACE", label: "play"}, {key: "L", label: "scores"}, {key: "H", label: "help"}, {key: "Q", label: "quit"}}
	if !a.sess.Guest && a.sess.Player.ID != 0 {
		hs = []keyHint{{key: "SPACE", label: "play"}, {key: "L", label: "scores"}, {key: "N", label: "name"}, {key: "H", label: "help"}, {key: "Q", label: "quit"}}
	}
	a.hints(in.X, r.Y+r.H-2, in.W, hs)

	a.drawStatusBar()
}

// drawStatusBar draws small status text on the ground.
func (a *App) drawStatusBar() {
	l := a.lay
	y := l.h - 1
	dark := hex(0x3b2914)
	var right string
	if a.opt.Online != nil {
		right = fmt.Sprintf("%d online", a.opt.Online())
	}
	if a.opt.ServerName != "" && l.w >= 70 {
		if right != "" {
			right += "  "
		}
		right += a.opt.ServerName
	}
	rw := 0
	if right != "" {
		right = a.trunc(right, l.w/2)
		rw = canvas.StringWidth(right)
		x := l.w - 1 - rw
		if a.opt.Online != nil {
			a.cv.Text(x-2, y, a.g.dot, hex(0x1f7a3a), canvas.Bold)
			rw += 2
		}
		a.cv.Text(x, y, right, dark, 0)
	}
	left := "C colors: " + a.color.String() + "  U glyphs: " + a.glyphs.String()
	if room := l.w - rw - 4; room >= 24 {
		a.cv.Text(1, y, a.trunc(left, room), dark, 0)
	}
}

func rankColor(rank int) rgb {
	switch rank {
	case 1:
		return colGold
	case 2:
		return colSilver
	case 3:
		return colBronze
	}
	return colMuted
}

// boardRow draws one leaderboard line: rank, name and score, with optional
// detail columns.
func (a *App) boardRow(x, y, w int, e store.Entry, details bool) {
	me := e.PlayerID == a.sess.Player.ID && e.PlayerID != 0
	if me {
		bg := colRowHi
		a.cv.Fill(canvas.Rect{X: x - 1, Y: y, W: w + 2, H: 1}, bg)
	}
	rank := fmt.Sprintf("%3d", e.Rank)
	a.cv.Text(x, y, rank, rankColor(e.Rank), canvas.Bold)
	nameX := x + 5
	scoreW := 6
	detailW := 0
	if details && w >= 52 {
		detailW = 16
	}
	nameW := w - 5 - scoreW - detailW - 1
	name := e.Name
	nameCol := colText
	if me {
		a.cv.Text(x+4, y, a.g.you, colAccent, canvas.Bold)
		nameCol = colAccent
	}
	sk := skins[int(e.PlayerID%int64(len(skins)))]
	if e.PlayerID <= 0 {
		sk = skins[0]
	}
	a.cv.Text(nameX, y, a.g.dot, sk.body, 0)
	n := a.cv.Text(nameX+2, y, a.trunc(name, nameW-2), nameCol, 0)
	if e.Kind == store.KindGuest && nameW-2-n > 7 {
		a.cv.Text(nameX+2+n+1, y, "guest", colFaint, 0)
	}
	a.cv.TextRight(x+w-detailW, y, strconv.Itoa(e.Score), colText, canvas.Bold)
	if detailW > 0 {
		a.cv.TextRight(x+w-8, y, strconv.Itoa(e.Runs), colMuted, 0)
		a.cv.TextRight(x+w, y, ago(a.now(), e.At), colMuted, 0)
	}
}

// ago formats a timestamp relative to now.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case t.Year() == now.Year():
		return t.Format("Jan 02")
	}
	return t.Format("2006-01")
}

func (a *App) drawPause() {
	l := a.lay
	w := min(l.playW-4, 44)
	h := 6
	r := canvas.Rect{X: l.playX + (l.playW-w)/2, Y: l.playTop + (l.playRows-h)/2, W: w, H: h}
	title := a.g.pause + " " + strings.ToUpper(a.pauseWhy)
	a.card(r, title, colCardBorder)
	msg := "Take a breather."
	if a.pauseWhy == "Resized" {
		msg = "The terminal changed size."
	}
	a.cv.TextCentered(r.X, r.Y+2, r.W, a.trunc(msg, r.W-4), colText, 0)
	a.hints(r.X+2, r.Y+3, r.W-4, []keyHint{{key: "SPACE", label: "resume"}, {key: "H", label: "help"}, {key: "Q", label: "quit"}})
}

// bigNumber draws digits with half-block pixels directly into the canvas.
func (a *App) bigNumber(cx, y int, s string, fg, bg rgb) {
	sx, sy := a.fontScale(1)
	pxw := digits3x5.textWidth(s, sx)
	var p canvas.Pixels
	p.Resize(pxw, digits3x5.height(sy)+1)
	p.FillRect(0, 0, p.W, p.H, bg)
	digits3x5.draw(&p, 0, 0, s, sx, sy, solid(fg), nil, nil)
	a.cv.DrawPixels(&p, cx-pxw/2, y, a.lay.ppc)
}

func (a *App) drawGameOver() {
	l := a.lay
	w := min(l.w-4, 46)
	digitRows := 3
	if l.ppc == 1 {
		digitRows = 6
	}
	h := 9 + digitRows
	r := canvas.Rect{X: (l.w - w) / 2, Y: max(0, (l.skyRows-h)/2), W: w, H: h}
	border := colCardBorder
	if a.newRecord {
		border = colGold
	}
	a.card(r, "GAME OVER", border)
	in := canvas.Rect{X: r.X + 2, Y: r.Y + 1, W: r.W - 4, H: r.H - 2}
	half := in.W / 2
	y := in.Y + 1
	a.cv.TextCentered(in.X, y, half, "SCORE", colMuted, canvas.Bold)
	a.cv.TextCentered(in.X+half, y, in.W-half, "BEST", colMuted, canvas.Bold)
	y++
	score := a.lastRun.Score
	a.bigNumber(in.X+half/2, y, strconv.Itoa(score), colText, colCard)
	bestCol := colAccent
	a.bigNumber(in.X+half+(in.W-half)/2, y, strconv.Itoa(a.Best()), bestCol, colCard)
	y += digitRows + 1

	switch {
	case a.newRecord:
		text := a.g.star + " NEW PERSONAL BEST " + a.g.star
		hue := []rgb{colGold, hex(0xff9ad5), colAccent2, colGood}
		a.cv.TextCentered(in.X, y, in.W, text, hue[int(a.t*6)%len(hue)], canvas.Bold)
	case score == 0:
		a.cv.TextCentered(in.X, y, in.W, "Tap SPACE in short bursts to stay airborne.", colMuted, 0)
	default:
		a.cv.TextCentered(in.X, y, in.W, crashLine(a.lastCrash), colMuted, 0)
	}
	y++
	if a.rank > 0 {
		a.cv.TextCentered(in.X, y, in.W, fmt.Sprintf("Leaderboard rank #%d of %d", a.rank, a.ranked), rankColor(min(a.rank, 4)).Lerp(colText, 0.2), canvas.Bold)
	}
	y++
	var status string
	col := colMuted
	switch a.save {
	case saveBusy:
		status, col = "Saving score"+a.g.ell, colMuted
	case saveRetrying:
		status, col = a.g.warn+" Storage busy, retrying"+a.g.ell, colWarn
	case saveOK:
		status, col = a.g.check+" Score saved", colGood
	case saveFailed:
		status, col = a.g.cross+" Not saved: "+a.saveErr, colBad
	case saveSkipped:
		status, col = a.g.cross+" Not saved: "+a.saveErr, colBad
	}
	a.cv.TextCentered(in.X, y, in.W, a.trunc(status, in.W), col, 0)
	ready := a.t-a.overAt > 0.6
	hs := []keyHint{{key: "SPACE", label: "retry", off: !ready}, {key: "L", label: "scores"}, {key: "Q", label: "quit"}}
	a.hints(in.X, r.Y+r.H-2, in.W, hs)
}

func crashLine(c game.Crash) string {
	if c == game.CrashGround {
		return "Grounded. Flap a little earlier."
	}
	return "Clipped a pipe. So close!"
}

func (a *App) drawBoard() {
	l := a.lay
	w := min(l.w-2, 78)
	h := l.h - 2
	r := canvas.Rect{X: (l.w - w) / 2, Y: 1, W: w, H: h}
	a.card(r, "LEADERBOARD", colCardBorder)
	in := canvas.Rect{X: r.X + 3, Y: r.Y + 1, W: r.W - 6, H: r.H - 2}

	// Optional side panel with the player's recent runs on wide screens.
	side := 0
	if w >= 76 && len(a.recent) > 0 {
		side = 22
	}
	tw := in.W - side
	y := in.Y + 1
	header := "  #  PILOT"
	a.cv.Text(in.X, y, header, colMuted, canvas.Bold)
	details := tw >= 52
	if details {
		a.cv.TextRight(in.X+tw-16, y, "BEST", colMuted, canvas.Bold)
		a.cv.TextRight(in.X+tw-8, y, "RUNS", colMuted, canvas.Bold)
		a.cv.TextRight(in.X+tw, y, "WHEN", colMuted, canvas.Bold)
	} else {
		a.cv.TextRight(in.X+tw, y, "BEST", colMuted, canvas.Bold)
	}
	y++
	a.cv.HLine(in.X, y, tw, []rune(a.g.hline)[0], colCardBorder)
	y++
	rows := min(a.boardRows(), a.opt.LeaderboardSize)
	switch {
	case a.boardErr:
		a.cv.Text(in.X, y, "The leaderboard is unavailable right now.", colWarn, 0)
	case len(a.board) == 0 && a.boardLoading:
		a.cv.Text(in.X, y, "Loading"+a.g.ell, colMuted, 0)
	case len(a.board) == 0:
		a.cv.Text(in.X, y, "No scores yet. Go set one!", colMuted, 0)
	}
	for i, e := range a.board {
		if i >= rows || y >= in.Y+in.H-2 {
			break
		}
		a.boardRow(in.X, y, tw, e, details)
		y++
	}

	if side > 0 {
		sx := in.X + tw + 3
		sy := in.Y + 1
		a.cv.Text(sx, sy, "YOUR RECENT RUNS", colMuted, canvas.Bold)
		sy += 2
		for _, rr := range a.recent {
			if sy >= in.Y+in.H-2 {
				break
			}
			a.cv.TextRight(sx+4, sy, strconv.Itoa(rr.Score), colText, canvas.Bold)
			a.cv.Text(sx+6, sy, a.trunc(ago(a.now(), rr.EndedAt), side-8), colMuted, 0)
			sy++
		}
	}

	footer := fmt.Sprintf("Page %d/%d", a.boardPage+1, a.boardPages())
	if a.rank > 0 {
		footer += fmt.Sprintf("   You: #%d of %d", a.rank, a.ranked)
	}
	a.cv.Text(in.X, r.Y+r.H-3, a.trunc(footer, in.W), colMuted, 0)
	a.hints(in.X, r.Y+r.H-2, in.W, []keyHint{{key: a.g.left + " " + a.g.right, label: "page"}, {key: "L", label: "back"}, {key: "Q", label: "quit"}})
}

func (a *App) drawHelp() {
	l := a.lay
	type line struct {
		key, text string
		head      bool
	}
	ident := "Guest: no SSH key was offered. Scores keep this guest name."
	if !a.sess.Guest {
		ident = "Signed in with key " + a.sess.KeyFingerprint
	}
	lines := []line{
		{"", "CONTROLS", true},
		{"SPACE", "flap / start / retry", false},
		{"P", "pause or resume", false},
		{"L", "leaderboard", false},
		{"N", "change your name (SSH key players)", false},
		{"C", "colors: " + a.color.String() + " (truecolor, 256, 16)", false},
		{"U", "graphics: " + a.glyphs.String() + " (unicode or ascii)", false},
		{"H ?", "this help", false},
		{"Q ESC", "quit (ESC goes back from menus)", false},
		{"", "", false},
		{"", "IDENTITY", true},
		{"", ident, false},
		{"", "", false},
		{"", "TIPS", true},
		{"", "Short taps beat long holds. Aim for the lower half of each gap.", false},
		{"", "If colors look wrong press C; if borders look broken press U.", false},
	}
	w := min(l.w-4, 70)
	maxLines := l.h - 4
	if len(lines) > maxLines {
		// Drop the tips first, then identity, keeping the controls.
		lines = lines[:max(0, min(len(lines), maxLines))]
	}
	h := len(lines) + 4
	r := canvas.Rect{X: (l.w - w) / 2, Y: max(0, (l.h-h)/2), W: w, H: h}
	a.card(r, "HELP", colCardBorder)
	y := r.Y + 2
	for _, ln := range lines {
		switch {
		case ln.head:
			a.cv.Text(r.X+3, y, ln.text, colMuted, canvas.Bold)
		case ln.key != "":
			a.cv.TextBg(r.X+3, y, " "+ln.key+" ", colKeyFg, colKeyBg, canvas.Bold)
			a.cv.Text(r.X+12, y, a.trunc(ln.text, r.W-15), colText, 0)
		default:
			a.cv.Text(r.X+3, y, a.trunc(ln.text, r.W-6), colText, 0)
		}
		y++
	}
}

func (a *App) drawName() {
	l := a.lay
	w := min(l.w-4, 48)
	h := 9
	r := canvas.Rect{X: (l.w - w) / 2, Y: max(0, (l.skyRows-h)/2), W: w, H: h}
	a.card(r, "CHOOSE YOUR NAME", colCardBorder)
	in := canvas.Rect{X: r.X + 3, Y: r.Y + 2, W: r.W - 6}
	field := canvas.Rect{X: in.X, Y: in.Y, W: in.W, H: 1}
	a.cv.Fill(field, hex(0x060a1c))
	cursor := ""
	if int(a.t/0.5)%2 == 0 && !a.nameBusy {
		cursor = a.g.cursor
	}
	a.cv.TextBg(field.X+1, field.Y, a.nameInput+cursor, colAccent, hex(0x060a1c), canvas.Bold)
	a.cv.Text(in.X, in.Y+2, a.trunc("3-16 characters: letters, digits, - _ .", in.W), colMuted, 0)
	switch {
	case a.nameBusy:
		a.cv.Text(in.X, in.Y+3, "Saving"+a.g.ell, colMuted, 0)
	case a.nameErr != "":
		a.cv.Text(in.X, in.Y+3, a.trunc(a.nameErr, in.W), colBad, 0)
	}
	a.hints(in.X, r.Y+r.H-2, in.W, []keyHint{{key: "ENTER", label: "save"}, {key: "ESC", label: "cancel"}})
}

func (a *App) drawNotice() {
	l := a.lay
	w := min(l.w-4, max(30, canvas.StringWidth(a.notice)+6))
	r := canvas.Rect{X: (l.w - w) / 2, Y: 1, W: w, H: 3}
	a.card(r, "NOTICE", colWarn)
	a.cv.TextCentered(r.X, r.Y+1, r.W, a.trunc(a.notice, r.W-4), colText, canvas.Bold)
}

func (a *App) drawTooSmall() {
	a.cv.Clear(hex(0x0d1330))
	need := fmt.Sprintf("%dx%d", a.opt.MinWidth, a.opt.MinHeight)
	have := fmt.Sprintf("%dx%d", a.w, a.h)
	lines := []struct {
		s    string
		c    rgb
		attr canvas.Attr
	}{
		{"FLAP", colAccent, canvas.Bold},
		{"", colText, 0},
		{"Terminal too small", colText, canvas.Bold},
		{"now " + have + ", need " + need, colWarn, 0},
		{"", colText, 0},
		{"Enlarge the window or zoom out", colMuted, 0},
		{"(Ctrl and minus in most terminals).", colMuted, 0},
		{"The game resumes automatically.", colMuted, 0},
	}
	if a.screen == screenPlay && a.world != nil && a.world.Phase() == game.Playing {
		lines = append(lines, struct {
			s    string
			c    rgb
			attr canvas.Attr
		}{"Your run is paused.", colGood, 0})
	}
	lines = append(lines, struct {
		s    string
		c    rgb
		attr canvas.Attr
	}{"Q to quit", colFaint, 0})
	// Drop blank lines first if the screen is very short.
	if len(lines) > a.h {
		var compact = lines[:0:0]
		for _, ln := range lines {
			if ln.s != "" {
				compact = append(compact, ln)
			}
		}
		lines = compact
	}
	top := max(0, (a.h-len(lines))/2)
	for i, ln := range lines {
		if top+i >= a.h {
			break
		}
		a.cv.TextCentered(0, top+i, a.w, a.trunc(ln.s, a.w), ln.c, ln.attr)
	}
}
