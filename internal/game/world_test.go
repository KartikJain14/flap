package game

import (
	"math"
	"testing"
)

func mustWorld(t *testing.T, p Params, seed uint64, view float64) *World {
	t.Helper()
	if err := p.Validate(); err != nil {
		t.Fatalf("invalid params: %v", err)
	}
	return NewWorld(p, seed, view)
}

func TestDefaultParamsAreValid(t *testing.T) {
	if err := DefaultParams().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsBrokenParams(t *testing.T) {
	cases := map[string]func(*Params){
		"tick rate":        func(p *Params) { p.TickRate = 0 },
		"nan gravity":      func(p *Params) { p.Gravity = math.NaN() },
		"no stages":        func(p *Params) { p.Stages = nil },
		"first stage":      func(p *Params) { p.Stages[0].MinScore = 3 },
		"unsorted":         func(p *Params) { p.Stages[2].MinScore = p.Stages[1].MinScore },
		"gap too small":    func(p *Params) { p.Stages[1].Gap = p.BirdHeight },
		"gap too large":    func(p *Params) { p.Stages[0].Gap = 0.85 },
		"spacing":          func(p *Params) { p.Stages[0].Spacing = p.PipeWidth },
		"impossible climb": func(p *Params) { p.Stages[4].GapShift = 0.6; p.Stages[4].Speed = 2.5 },
		"bird x":           func(p *Params) { p.BirdX = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := DefaultParams()
			mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestGravityAndFlap(t *testing.T) {
	p := DefaultParams()
	w := mustWorld(t, p, 1, 1.8)
	y0 := w.Bird().Y
	// Fall for a quarter of a second; the speed cap is not reached yet.
	n := p.TickRate / 4
	for range n {
		w.Step()
	}
	dt := p.Dt()
	// Semi-implicit Euler: y = g*dt²*n(n+1)/2.
	want := p.Gravity * dt * dt * float64(n*(n+1)) / 2
	if got := w.Bird().Y - y0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("fall distance = %.6f, want %.6f", got, want)
	}
	if !w.Flap() {
		t.Fatal("flap not applied while playing")
	}
	if got := w.Bird().VY; got != -p.FlapVelocity {
		t.Fatalf("velocity after flap = %v, want %v", got, -p.FlapVelocity)
	}
	before := w.Bird().Y
	w.Step()
	if w.Bird().Y >= before {
		t.Fatal("bird did not rise after flapping")
	}
	if w.Flaps() != 1 {
		t.Fatalf("flaps = %d", w.Flaps())
	}
}

func TestFallSpeedIsCapped(t *testing.T) {
	p := DefaultParams()
	w := mustWorld(t, p, 1, 1.8)
	w.bird.Y = 0.05
	for range p.TickRate * 2 {
		if w.Phase() != Playing {
			break
		}
		w.Step()
		if w.Bird().VY > p.MaxFallSpeed+1e-12 {
			t.Fatalf("vy %v exceeds cap %v", w.Bird().VY, p.MaxFallSpeed)
		}
	}
}

func TestCeilingClampsWithoutKilling(t *testing.T) {
	p := DefaultParams()
	p.FirstPipe = 5 // keep pipes out of the way
	w := mustWorld(t, p, 1, 1.8)
	for i := 0; i < p.TickRate; i++ {
		w.Flap()
		w.Step()
		if w.Bird().Y < p.BirdHeight/2-1e-12 {
			t.Fatalf("bird left the ceiling: y=%v", w.Bird().Y)
		}
	}
	if w.Phase() != Playing {
		t.Fatalf("phase = %v, want playing", w.Phase())
	}
}

func TestGroundCollisionEndsRun(t *testing.T) {
	p := DefaultParams()
	w := mustWorld(t, p, 1, 1.8)
	var events []Event
	for i := 0; i < p.TickRate*3 && w.Phase() != Over; i++ {
		w.Step()
		events = append(events, w.Events()...)
	}
	if w.Phase() != Over || w.Crash() != CrashGround {
		t.Fatalf("phase=%v crash=%v, want over/ground", w.Phase(), w.Crash())
	}
	if !hasEvent(events, EventCrash) || !hasEvent(events, EventLanded) {
		t.Fatalf("missing crash/landed events: %+v", events)
	}
	if w.Flap() {
		t.Fatal("flap accepted after the run ended")
	}
	ticks := w.Ticks()
	w.Step()
	if w.Ticks() != ticks {
		t.Fatal("world kept ticking after the run ended")
	}
}

func TestPipeCollision(t *testing.T) {
	p := DefaultParams()
	w := mustWorld(t, p, 7, 1.8)
	first := w.Pipes()[0]
	// Place the bird inside the top pipe right at its left edge.
	w.camera = first.X - p.BirdX + p.BirdWidth/2
	w.bird.Y = first.GapTop() - 0.05
	w.bird.VY = 0
	w.Step()
	if w.Phase() != Dying || w.Crash() != CrashPipe {
		t.Fatalf("phase=%v crash=%v, want dying/pipe", w.Phase(), w.Crash())
	}
	for i := 0; i < p.TickRate*3 && w.Phase() != Over; i++ {
		w.Step()
	}
	if w.Phase() != Over {
		t.Fatal("dying bird never landed")
	}
	if w.Bird().Y > 1-p.BirdHeight/2+1e-9 {
		t.Fatalf("bird fell through the ground: %v", w.Bird().Y)
	}
}

func TestBirdInGapDoesNotCollide(t *testing.T) {
	p := DefaultParams()
	w := mustWorld(t, p, 7, 1.8)
	first := w.Pipes()[0]
	w.camera = first.X - p.BirdX + p.PipeWidth/2
	w.bird.Y = first.GapY
	if c := w.collision(); c != CrashNone {
		t.Fatalf("collision %v with bird centred in the gap", c)
	}
}

func TestEllipseHitsRect(t *testing.T) {
	inf := math.Inf(1)
	cases := []struct {
		name           string
		cx, cy, rx, ry float64
		x0, y0, x1, y1 float64
		want           bool
	}{
		{"inside", 0, 0, 1, 0.5, -0.1, -0.1, 0.1, 0.1, true},
		{"left of", -2, 0, 1, 0.5, 0, -1, 1, 1, false},
		{"touching edge", -1, 0, 1, 0.5, 0, -1, 1, 1, false},
		{"overlapping edge", -0.9, 0, 1, 0.5, 0, -1, 1, 1, true},
		{"below infinite", 0, 0.6, 1, 0.5, -1, 0, 1, -inf, false},
		{"corner miss", -0.9, -0.45, 1, 0.5, 0, 0, 1, 1, false},
		{"corner hit", -0.5, -0.2, 1, 0.5, 0, 0, 1, 1, true},
		{"infinite rect", 0, 10, 0.1, 0.1, -1, -inf, 1, inf, true},
	}
	for _, c := range cases {
		if got := EllipseHitsRect(c.cx, c.cy, c.rx, c.ry, c.x0, c.y0, c.x1, c.y1); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCourseIsFairAndBounded(t *testing.T) {
	p := DefaultParams()
	for seed := uint64(0); seed < 200; seed++ {
		w := mustWorld(t, p, seed, 50) // a huge view materialises many pipes
		pipes := w.Pipes()
		if len(pipes) < 40 {
			t.Fatalf("seed %d: only %d pipes materialised", seed, len(pipes))
		}
		for i, pp := range pipes {
			st := p.Stages[w.stageFor(pp.Index)]
			if pp.Gap != st.Gap {
				t.Fatalf("seed %d pipe %d: gap %v, want %v", seed, i, pp.Gap, st.Gap)
			}
			if pp.GapTop() < p.GapMargin-1e-9 || pp.GapBottom() > 1-p.GapMargin+1e-9 {
				t.Fatalf("seed %d pipe %d: gap [%v,%v] outside margins", seed, i, pp.GapTop(), pp.GapBottom())
			}
			if i == 0 {
				continue
			}
			prev := pipes[i-1]
			if d := math.Abs(pp.GapY - prev.GapY); d > st.GapShift+1e-9 {
				t.Fatalf("seed %d pipe %d: gap moved %v > %v", seed, i, d, st.GapShift)
			}
			if sp := pp.X - prev.X; math.Abs(sp-st.Spacing) > 1e-9 {
				t.Fatalf("seed %d pipe %d: spacing %v, want %v", seed, i, sp, st.Spacing)
			}
		}
	}
}

// script is a deterministic input sequence: tick numbers at which to flap.
func runScripted(p Params, seed uint64, view float64, flaps map[int]bool, maxTicks int) *World {
	w := NewWorld(p, seed, view)
	for tick := 0; tick < maxTicks && w.Phase() != Over; tick++ {
		if flaps[tick] {
			w.Flap()
		}
		w.Step()
	}
	return w
}

func TestDeterminismIndependentOfViewWidth(t *testing.T) {
	p := DefaultParams()
	// Record a bot run once, then replay its inputs with other view widths.
	flaps := map[int]bool{}
	w := NewWorld(p, 42, 1.8)
	for tick := 0; tick < p.TickRate*60 && w.Phase() == Playing; tick++ {
		if botWants(w, 0) {
			w.Flap()
			flaps[tick] = true
		}
		w.Step()
	}
	if w.Score() < 5 {
		t.Fatalf("bot only scored %d", w.Score())
	}
	for _, view := range []float64{1.35, 1.8, 2.2, 4} {
		r := runScripted(p, 42, view, flaps, p.TickRate*60)
		if r.Score() != w.Score() || r.Ticks() != w.Ticks() || r.Bird() != w.Bird() {
			t.Fatalf("view %.2f diverged: score %d/%d ticks %d/%d", view, r.Score(), w.Score(), r.Ticks(), w.Ticks())
		}
	}
	// Changing the view width mid-run must not change the outcome either.
	r := NewWorld(p, 42, 1.35)
	for tick := 0; tick < p.TickRate*60 && r.Phase() != Over; tick++ {
		if tick%97 == 0 {
			r.SetViewWidth([]float64{1.35, 2.2, 1.6}[tick%3])
		}
		if flaps[tick] {
			r.Flap()
		}
		r.Step()
	}
	if r.Score() != w.Score() || r.Ticks() != w.Ticks() {
		t.Fatalf("resizing diverged: score %d/%d ticks %d/%d", r.Score(), w.Score(), r.Ticks(), w.Ticks())
	}
}

func TestScoreCountsEachPipeOnce(t *testing.T) {
	p := DefaultParams()
	w := NewWorld(p, 3, 1.8)
	scoreEvents := 0
	passed := map[int]bool{}
	for tick := 0; tick < p.TickRate*90 && w.Phase() == Playing; tick++ {
		if botWants(w, 0) {
			w.Flap()
		}
		w.Step()
		for _, e := range w.Events() {
			if e.Kind == EventScore {
				scoreEvents++
				if e.Score != scoreEvents {
					t.Fatalf("score event %d carries score %d", scoreEvents, e.Score)
				}
			}
		}
		for _, pp := range w.Pipes() {
			if pp.X+p.PipeWidth/2 <= w.BirdWorldX() {
				passed[pp.Index] = true
			}
		}
	}
	if w.Score() != scoreEvents || w.Score() != len(passed) {
		t.Fatalf("score %d, events %d, pipes passed %d", w.Score(), scoreEvents, len(passed))
	}
	if w.Score() < 20 {
		t.Fatalf("bot scored only %d", w.Score())
	}
}

func TestDifficultyProgression(t *testing.T) {
	p := DefaultParams()
	w := NewWorld(p, 11, 1.8)
	var stages []Event
	lastSpeed := w.Speed()
	for tick := 0; tick < p.TickRate*400 && w.Phase() == Playing && w.Score() < 120; tick++ {
		if botWants(w, 0) {
			w.Flap()
		}
		w.Step()
		for _, e := range w.Events() {
			if e.Kind == EventStage {
				stages = append(stages, e)
			}
		}
		if w.Speed() < lastSpeed {
			t.Fatal("speed decreased")
		}
		lastSpeed = w.Speed()
	}
	if w.Score() < 120 {
		t.Fatalf("bot reached only %d; cannot check all stages", w.Score())
	}
	if len(stages) != len(p.Stages)-1 {
		t.Fatalf("got %d stage events, want %d", len(stages), len(p.Stages)-1)
	}
	for i, e := range stages {
		if e.Stage != i+1 || e.Score != p.Stages[i+1].MinScore {
			t.Fatalf("stage event %d = %+v", i, e)
		}
	}
}

// TestPlayableWithNetworkLatency shows that a simple bot reacting 120 ms late
// (a slow SSH round trip) still progresses through the course. This guards
// against tuning changes that would make the game unfair over SSH.
func TestPlayableWithNetworkLatency(t *testing.T) {
	p := DefaultParams()
	delay := p.TickRate * 120 / 1000
	total, low := 0, 0
	const seeds = 30
	for seed := uint64(100); seed < 100+seeds; seed++ {
		w := NewWorld(p, seed, 1.8)
		// pending holds decisions that are still "in flight" to the server.
		pending := make([]bool, delay)
		for tick := 0; tick < p.TickRate*120 && w.Phase() == Playing; tick++ {
			// The player sees a stale frame and anticipates where the bird
			// will be once the key press arrives.
			future := predictBird(p, w.Bird(), pending)
			pending = append(pending, botWantsAt(w, future, float64(delay)*p.Dt()))
			if pending[0] {
				w.Flap()
			}
			pending = pending[1:]
			w.Step()
		}
		total += w.Score()
		if w.Score() < 10 {
			low++
		}
	}
	avg := float64(total) / seeds
	t.Logf("latency bot: average score %.1f, %d/%d runs below 10", avg, low, seeds)
	if avg < 25 {
		t.Fatalf("latency bot average score %.1f is too low; the game may be unfair over SSH", avg)
	}
	if low > seeds/5 {
		t.Fatalf("%d of %d latency runs ended before score 10", low, seeds)
	}
}

// predictBird integrates the bird forward, applying the flaps in pending.
func predictBird(p Params, b Bird, pending []bool) Bird {
	dt := p.Dt()
	for _, flap := range pending {
		if flap {
			b.VY = -p.FlapVelocity
		}
		b.VY = math.Min(b.VY+p.Gravity*dt, p.MaxFallSpeed)
		b.Y = math.Max(b.Y+b.VY*dt, p.BirdHeight/2)
	}
	return b
}

func botWants(w *World, _ float64) bool { return botWantsAt(w, w.Bird(), 0) }

// botWantsAt is a simple autopilot: flap when the bird (predicted state b,
// ahead seconds in the future) has sunk below the next gap's aim point.
func botWantsAt(w *World, b Bird, ahead float64) bool {
	p := w.Params()
	bx := w.BirdWorldX() + w.Speed()*ahead
	var target *Pipe
	for i := range w.Pipes() {
		pp := &w.Pipes()[i]
		if pp.X+p.PipeWidth+p.BirdWidth/2 > bx {
			target = pp
			break
		}
	}
	aim := 0.5
	if target != nil {
		aim = target.GapBottom() - p.BirdHeight/2 - target.Gap*0.18
	}
	return b.Y > aim && b.VY > -p.FlapVelocity*0.25
}

func hasEvent(evs []Event, k EventKind) bool {
	for _, e := range evs {
		if e.Kind == k {
			return true
		}
	}
	return false
}
