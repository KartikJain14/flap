package game

import (
	"math"
	"math/rand/v2"
)

// Phase is the lifecycle state of a run.
type Phase uint8

const (
	// Playing means the bird is alive and the world scrolls.
	Playing Phase = iota
	// Dying means the bird crashed and is falling to the ground.
	Dying
	// Over means the run has finished.
	Over
)

func (p Phase) String() string {
	switch p {
	case Playing:
		return "playing"
	case Dying:
		return "dying"
	case Over:
		return "over"
	}
	return "unknown"
}

// Crash describes what ended a run.
type Crash uint8

const (
	CrashNone Crash = iota
	CrashPipe
	CrashGround
)

func (c Crash) String() string {
	switch c {
	case CrashPipe:
		return "pipe"
	case CrashGround:
		return "ground"
	}
	return "none"
}

// EventKind identifies something noteworthy that happened during a step.
type EventKind uint8

const (
	EventFlap EventKind = iota + 1
	EventScore
	EventStage
	EventCrash
	EventLanded
)

// Event is emitted by the simulation so the presentation layer can react
// with effects. Events never feed back into the simulation.
type Event struct {
	Kind  EventKind
	Score int
	Stage int
}

// Bird is the player-controlled entity.
type Bird struct {
	// Y is the vertical position of the bird's centre.
	Y float64
	// VY is the vertical velocity; negative values move up.
	VY float64
	// SinceFlap is the time in seconds since the last flap, for animation.
	SinceFlap float64
}

// Pipe is one pipe pair. X is its left edge in absolute world coordinates.
type Pipe struct {
	Index  int
	X      float64
	GapY   float64
	Gap    float64
	Scored bool
}

// GapTop returns the y coordinate of the top edge of the opening.
func (p Pipe) GapTop() float64 { return p.GapY - p.Gap/2 }

// GapBottom returns the y coordinate of the bottom edge of the opening.
func (p Pipe) GapBottom() float64 { return p.GapY + p.Gap/2 }

// World is a single run of the game.
type World struct {
	params Params
	dt     float64
	seed   uint64
	rng    *rand.Rand

	phase  Phase
	crash  Crash
	bird   Bird
	pipes  []Pipe
	camera float64
	view   float64

	nextIndex int
	nextX     float64
	lastGapY  float64

	score      int
	stage      int
	ticks      int
	flaps      int
	dyingTicks int

	events []Event
}

// NewWorld starts a new run. The parameters must have been validated.
// viewWidth is the visible width in world units; it only controls how far
// ahead pipes are materialised and never changes the course itself.
func NewWorld(p Params, seed uint64, viewWidth float64) *World {
	w := &World{
		params: p,
		dt:     p.Dt(),
		seed:   seed,
		rng:    rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		view:   viewWidth,
		bird:   Bird{Y: 0.45},
		pipes:  make([]Pipe, 0, 8),
	}
	w.nextX = p.BirdX + p.FirstPipe
	w.lastGapY = 0.5
	w.spawnPipes()
	return w
}

// Params returns the parameters the world was built with.
func (w *World) Params() Params { return w.params }

// Seed returns the seed of the run's course.
func (w *World) Seed() uint64 { return w.seed }

// Phase returns the current lifecycle phase.
func (w *World) Phase() Phase { return w.phase }

// Crash returns what ended the run, if anything.
func (w *World) Crash() Crash { return w.crash }

// Bird returns a copy of the bird state.
func (w *World) Bird() Bird { return w.bird }

// Pipes returns the currently materialised pipes. The slice is owned by the
// world and is only valid until the next call to Step.
func (w *World) Pipes() []Pipe { return w.pipes }

// Camera returns the absolute world x coordinate of the view's left edge.
func (w *World) Camera() float64 { return w.camera }

// BirdWorldX returns the absolute world x coordinate of the bird's centre.
func (w *World) BirdWorldX() float64 { return w.camera + w.params.BirdX }

// Score returns the number of pipe pairs passed.
func (w *World) Score() int { return w.score }

// Stage returns the index of the active difficulty stage.
func (w *World) Stage() int { return w.stage }

// Ticks returns the number of simulation steps spent playing.
func (w *World) Ticks() int { return w.ticks }

// Flaps returns the number of flaps performed.
func (w *World) Flaps() int { return w.flaps }

// Duration returns the simulated play time in seconds.
func (w *World) Duration() float64 { return float64(w.ticks) * w.dt }

// Speed returns the current scroll speed.
func (w *World) Speed() float64 { return w.params.StageAt(w.score).Speed }

// ViewWidth returns the visible width in world units.
func (w *World) ViewWidth() float64 { return w.view }

// SetViewWidth updates the visible width, for example after a terminal
// resize. The course is unaffected; only pipe materialisation changes.
func (w *World) SetViewWidth(v float64) {
	w.view = v
	w.spawnPipes()
}

// Events returns the events emitted since the previous call and clears them.
func (w *World) Events() []Event {
	ev := w.events
	w.events = w.events[:0:0]
	return ev
}

// Flap gives the bird an upward impulse. It reports whether the flap was
// applied, which only happens while playing.
func (w *World) Flap() bool {
	if w.phase != Playing {
		return false
	}
	w.bird.VY = -w.params.FlapVelocity
	w.bird.SinceFlap = 0
	w.flaps++
	w.emit(Event{Kind: EventFlap})
	return true
}

// Step advances the simulation by one fixed tick.
func (w *World) Step() {
	switch w.phase {
	case Playing:
		w.ticks++
		w.camera += w.Speed() * w.dt
		w.integrate()
		w.spawnPipes()
		w.despawnPipes()
		w.updateScore()
		if c := w.collision(); c != CrashNone {
			w.crash = c
			w.phase = Dying
			if c == CrashGround {
				w.landed()
				return
			}
			w.emit(Event{Kind: EventCrash, Score: w.score})
		}
	case Dying:
		w.dyingTicks++
		w.integrate()
		ground := 1 - w.params.BirdHeight/2
		if w.bird.Y >= ground || float64(w.dyingTicks)*w.dt > 2 {
			w.bird.Y = math.Min(w.bird.Y, ground)
			w.landed()
		}
	case Over:
	}
}

func (w *World) landed() {
	if w.phase == Over {
		return
	}
	w.bird.Y = math.Min(w.bird.Y, 1-w.params.BirdHeight/2)
	w.bird.VY = 0
	if w.crash == CrashGround {
		w.emit(Event{Kind: EventCrash, Score: w.score})
	}
	w.phase = Over
	w.emit(Event{Kind: EventLanded, Score: w.score})
}

func (w *World) emit(e Event) { w.events = append(w.events, e) }

func (w *World) integrate() {
	b := &w.bird
	b.SinceFlap += w.dt
	b.VY = math.Min(b.VY+w.params.Gravity*w.dt, w.params.MaxFallSpeed)
	b.Y += b.VY * w.dt
	if top := w.params.BirdHeight / 2; b.Y < top {
		b.Y = top
		if b.VY < 0 {
			b.VY = 0
		}
	}
}

// stageFor returns the stage index that applies at the given score. Pipe i is
// reached when the score equals i, so the course can be generated from pipe
// indices alone, independent of timing or view width.
func (w *World) stageFor(score int) int { return w.params.StageIndex(score) }

func (w *World) spawnPipes() {
	limit := w.camera + w.view + 2*w.params.PipeWidth
	for w.nextX < limit {
		st := w.params.StageAt(w.nextIndex)
		lo := w.params.GapMargin + st.Gap/2
		hi := 1 - w.params.GapMargin - st.Gap/2
		r := w.rng.Float64()*2 - 1
		var y float64
		if w.nextIndex == 0 {
			y = 0.5 + r*st.GapShift/2
		} else {
			y = w.lastGapY + r*st.GapShift
		}
		y = math.Max(lo, math.Min(hi, y))
		w.pipes = append(w.pipes, Pipe{Index: w.nextIndex, X: w.nextX, GapY: y, Gap: st.Gap})
		w.lastGapY = y
		w.nextIndex++
		w.nextX += w.params.StageAt(w.nextIndex).Spacing
	}
}

func (w *World) despawnPipes() {
	n := 0
	for _, p := range w.pipes {
		if p.X+w.params.PipeWidth < w.camera-0.25 {
			n++
			continue
		}
		break
	}
	if n > 0 {
		w.pipes = append(w.pipes[:0], w.pipes[n:]...)
	}
}

func (w *World) updateScore() {
	bx := w.BirdWorldX()
	for i := range w.pipes {
		p := &w.pipes[i]
		if p.Scored || p.X+w.params.PipeWidth/2 > bx {
			continue
		}
		p.Scored = true
		w.score++
		w.emit(Event{Kind: EventScore, Score: w.score})
		if s := w.stageFor(w.score); s != w.stage {
			w.stage = s
			w.emit(Event{Kind: EventStage, Score: w.score, Stage: s})
		}
	}
}

func (w *World) collision() Crash {
	rx, ry := w.params.BirdWidth/2, w.params.BirdHeight/2
	bx, by := w.BirdWorldX(), w.bird.Y
	if by+ry >= 1 {
		return CrashGround
	}
	for _, p := range w.pipes {
		x0, x1 := p.X, p.X+w.params.PipeWidth
		if x1 < bx-rx || x0 > bx+rx {
			continue
		}
		if EllipseHitsRect(bx, by, rx, ry, x0, math.Inf(-1), x1, p.GapTop()) ||
			EllipseHitsRect(bx, by, rx, ry, x0, p.GapBottom(), x1, math.Inf(1)) {
			return CrashPipe
		}
	}
	return CrashNone
}

// EllipseHitsRect reports whether an axis-aligned ellipse centred at (cx, cy)
// with radii (rx, ry) overlaps the rectangle [x0,x1]×[y0,y1]. The rectangle may
// extend to infinity. Touching edges do not count as an overlap.
func EllipseHitsRect(cx, cy, rx, ry, x0, y0, x1, y1 float64) bool {
	// Scale the x axis so the ellipse becomes a circle of radius ry.
	s := ry / rx
	px := math.Max(x0, math.Min(cx, x1))
	py := math.Max(y0, math.Min(cy, y1))
	dx := (cx - px) * s
	dy := cy - py
	return dx*dx+dy*dy < ry*ry
}
