package tui

import (
	"context"
	"fmt"
	"math"
	"strings"

	tea "charm.land/bubbletea/v2"

	"flap/internal/canvas"
	"flap/internal/game"
	"flap/internal/store"
)

func (a *App) spendWings() tea.Cmd {
	a.wingsBusy, a.wingsErr = true, ""
	st, resolve, player := a.st, a.sess.Resolve, a.sess.Player
	runID, retry, timeout := a.runID, a.retries, a.opt.StoreTimeout
	pipes := a.world.CollectedWings()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if player.ID == 0 {
			if resolve == nil {
				return wingsMsg{runID: runID, err: errNoStorage}
			}
			var err error
			player, err = resolve(ctx)
			if err != nil {
				return wingsMsg{runID: runID, err: err}
			}
		}
		if _, err := st.CollectWings(ctx, player.ID, runID, pipes); err != nil {
			return wingsMsg{runID: runID, player: &player, err: err}
		}
		balance, err := st.SpendWings(ctx, player.ID, runID, retry)
		return wingsMsg{runID: runID, player: &player, balance: balance, err: err}
	}
}

func (a *App) onWings(msg wingsMsg) {
	if msg.runID != a.runID || !a.continueOffer {
		return
	}
	a.wingsBusy, a.dirty = false, true
	if msg.player != nil && a.sess.Player.ID == 0 {
		a.sess.Player = *msg.player
		a.sess.StorageDown = false
	}
	if msg.err != nil {
		a.wingsErr = "Couldn't use wings. W to try again."
		if msg.err == store.ErrNoWings {
			a.sess.Player.Wings = msg.balance
			a.wingsErr = "Not enough wings. SPACE to restart."
		}
		a.log.Warn("continue purchase failed", "err", msg.err)
		return
	}
	a.sess.Player.Wings = msg.balance
	if a.world.Phase() != game.Over || !a.world.Revive() {
		return
	}
	a.retries++
	a.continueOffer = false
	a.paused, a.pauseWhy = false, ""
	a.acc, a.countdown, a.birdAngle = 0, 1.5, -15
	a.fx.list = a.fx.list[:0]
	a.showBanner("WINGS AWAKENED", "3 seconds of protection - keep flapping!")
	a.ringBell()
	x, y := a.birdPixel()
	for i := 0; i < 40; i++ {
		ang := float64(i) * 2 * math.Pi / 40
		a.fx.add(particle{x: x, y: y, vx: math.Cos(ang) * 45, vy: math.Sin(ang) * 30, ttl: 1.5, col: colGold})
	}
}

func (a *App) drawContinue() {
	l := a.lay
	w, h := min(l.playW-2, 56), 10
	r := canvas.Rect{X: l.playX + (l.playW-w)/2, Y: l.playTop + max(0, (l.playRows-h)/2), W: w, H: h}
	a.cv.Shade(a.cv.Bounds(), 0.35)
	a.card(r, "SPREAD YOUR WINGS", colGold)
	lines := []string{
		fmt.Sprintf("Score %d - keep this flight going?", a.world.Score()),
		fmt.Sprintf("WINGS %d   /   CONTINUE COST %d", a.sess.Player.Wings, store.ContinueCost(a.retries)),
		"3s invincible. Retry cost doubles.",
		fmt.Sprintf("Collect glowing wings every %d pipes.", game.WingInterval),
	}
	for i, s := range lines {
		a.cv.TextCentered(r.X+2, r.Y+1+i, r.W-4, a.trunc(s, r.W-4), colText, 0)
	}
	a.cv.TextCentered(r.X+2, r.Y+5, r.W-4, fmt.Sprintf("CONTINUE? %d", int(math.Ceil(a.continueLeft))), colGold, canvas.Bold)
	filled := min(20, max(0, int(math.Ceil(20*a.continueLeft/5))))
	a.cv.TextCentered(r.X+2, r.Y+7, r.W-4, strings.Repeat("=", filled)+strings.Repeat("-", 20-filled), colGold, 0)
	status := a.wingsErr
	if a.wingsBusy {
		status = "Unfurling your wings..."
	}
	a.cv.TextCentered(r.X+2, r.Y+6, r.W-4, a.trunc(status, r.W-4), colWarn, 0)
	a.hints(r.X+2, r.Y+8, r.W-4, []keyHint{{key: "W", label: "continue", off: a.wingsBusy}, {key: "SPACE", label: "new game", off: a.wingsBusy}})
}

// drawShield draws a pulsing halo and outstretched feather trails. All effects
// use the normal pixel canvas, including in ASCII and 256-colour terminals.
func (a *App) drawShield() {
	x, y := a.birdPixel()
	r := 4 + math.Sin(a.t*9)
	for i := 0; i < 24; i++ {
		ang := float64(i) * 2 * math.Pi / 24
		a.scene.px.Set(int(x+math.Cos(ang)*r*1.6), int(y+math.Sin(ang)*r), colGold)
	}
	for side := -1; side <= 1; side += 2 {
		for i := 1; i <= 6; i++ {
			a.scene.px.Set(int(x)+side*(3+i), int(y)-i/2-int(1+math.Sin(a.t*12)), colGold)
		}
	}
}

func (a *App) collectCmd() tea.Cmd {
	st, resolve, player := a.st, a.sess.Resolve, a.sess.Player
	if st == nil {
		return nil
	}
	runID, retries, timeout, pipes := a.runID, a.retries, a.opt.StoreTimeout, a.world.CollectedWings()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if player.ID == 0 {
			if resolve == nil {
				return pickupMsg{runID: runID, err: errNoStorage}
			}
			var err error
			player, err = resolve(ctx)
			if err != nil {
				return pickupMsg{runID: runID, err: err}
			}
		}
		balance, err := st.CollectWings(ctx, player.ID, runID, pipes)
		return pickupMsg{runID: runID, retries: retries, balance: balance, player: &player, err: err}
	}
}

func (a *App) onPickup(msg pickupMsg) {
	if msg.player != nil && a.sess.Player.ID == 0 {
		a.sess.Player = *msg.player
		a.sess.StorageDown = false
	}
	if msg.err != nil {
		a.log.Warn("saving wing pickup failed; will recover on purchase or final save", "err", msg.err)
		return
	}
	if msg.runID == a.runID && msg.retries == a.retries && !a.wingsBusy {
		a.sess.Player.Wings = msg.balance
	}
	a.dirty = true
}

// drawWingPickups uses mirrored feather silhouettes so the collectible reads
// as a pair of angel wings at small terminal sizes, with a gold aura behind it.
func (a *App) drawWingPickups() {
	l, px := a.lay, &a.scene.px
	for _, p := range a.world.Pipes() {
		if !p.HasWing() {
			continue
		}
		x := l.worldX(p.X + a.opt.Params.PipeWidth/2 - a.world.Camera())
		y := l.worldY(p.GapY)
		width := max(11, int(math.Round(l.unitX*0.28)))
		height := max(4*l.ppc, int(math.Round(l.unitY*0.16)))
		if x+float64(width) < 0 || x-float64(width) > float64(l.w) {
			continue
		}
		a.drawAngelWings(px, x, y, width, height)
	}
}

// Each row is the left wing; the right is mirrored around a clear centre.
// The stepped lower edge separates the long flight feathers.
var angelWing = [...]string{
	"#       ",
	"##      ",
	"###     ",
	"####### ",
	" #######",
	" # #####",
	"  # ####",
	"   # ###",
	"     ## ",
}

func (a *App) drawAngelWings(px *canvas.Pixels, x, y float64, width, height int) {
	if width%2 == 0 {
		width++
	}
	half := width / 2
	rx, ry := float64(half+2), float64(height)/2+1
	animT := math.Floor(a.t*6) / 6
	for cy := int(y - ry*2); cy <= int(y+ry*1.3); cy++ {
		for cx := int(x - rx*1.6); cx <= int(x+rx*1.3); cx++ {
			dx, dy := (float64(cx)-x)/rx, (float64(cy)-y)/ry
			d := math.Sqrt(dx*dx + dy*dy)
			// Three soft bands keep the aura warm and readable without
			// generating a new terminal colour for every pixel each frame.
			switch {
			case d < 0.85:
				px.Blend(cx, cy, hex(0xffc34a), 0.8)
			case d < 1.05:
				px.Blend(cx, cy, hex(0xffb32b), 0.4)
			case d < 1.3:
				px.Blend(cx, cy, hex(0xffad20), 0.15)
			}
		}
	}
	mask := func(col, row int) bool {
		if col < 0 || col >= 8 || row < 0 || row >= len(angelWing) {
			return false
		}
		return angelWing[row][col] == '#'
	}
	for dy := 0; dy < height; dy++ {
		row := dy * len(angelWing) / height
		for side := -1; side <= 1; side += 2 {
			for dx := 1; dx <= half; dx++ {
				col := (half - dx) * 8 / half
				if !mask(col, row) {
					continue
				}
				c := hex(0xfffff4)
				if !mask(col, row-1) || !mask(col-1, row) {
					c = hex(0xffffff)
				}
				if !mask(col, row+1) || !mask(col+1, row) {
					c = hex(0xffdf92)
				}
				if mask(col, row-1) && (row+col)%4 == 0 {
					c = hex(0xe8e3cc)
				}
				px.Set(int(math.Round(x))+side*dx, int(math.Round(y))+dy-height/2, c)
			}
		}
	}
	// A small golden halo joins the pair visually without filling the gap
	// between the two white feathered wings.
	for i := 0; i < 16; i++ {
		ang := float64(i) * 2 * math.Pi / 16
		px.Set(int(math.Round(x+math.Cos(ang)*2)), int(math.Round(y-float64(height)/2-1+math.Sin(ang))), colGold)
	}
	for side := -1; side <= 1; side += 2 {
		sx := int(math.Round(x)) + side*(half+2)
		sy := int(math.Round(y)) + int(math.Sin(animT*4+float64(side))*ry/2)
		px.Set(sx, sy, hex(0xfff8d6))
		if int(animT*4)%2 == 0 {
			px.Set(sx-1, sy, colGold)
			px.Set(sx+1, sy, colGold)
			px.Set(sx, sy-1, colGold)
			px.Set(sx, sy+1, colGold)
		}
	}
}
