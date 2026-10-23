// Package game implements the Flap simulation: physics, pipe generation,
// collision detection, scoring and difficulty progression.
//
// The simulation is pure and deterministic. It performs no I/O, reads no
// clocks and owns its own seeded random number generator, so a run is fully
// determined by its parameters, its seed and the tick numbers at which the
// player flapped. Rendering and timing live in other packages.
//
// All positions are expressed in world units where the playable height (from
// the ceiling to the top of the ground) is exactly 1.0 and y grows downwards.
// Horizontal distances use the same unit, so the world can be mapped onto any
// terminal size without changing the physics.
package game

import (
	"errors"
	"fmt"
	"math"
)

// Stage is one step of the difficulty curve.
type Stage struct {
	// MinScore is the score at which this stage begins.
	MinScore int `toml:"min_score"`
	// Speed is the horizontal scroll speed in world units per second.
	Speed float64 `toml:"speed"`
	// Gap is the height of the opening between a pipe pair.
	Gap float64 `toml:"gap"`
	// Spacing is the horizontal distance between consecutive pipe pairs.
	Spacing float64 `toml:"spacing"`
	// GapShift is the largest change of the gap centre between two
	// consecutive pipe pairs.
	GapShift float64 `toml:"gap_shift"`
}

// Params configures the simulation. Use DefaultParams as a starting point and
// call Validate before constructing a World.
type Params struct {
	// TickRate is the number of fixed simulation steps per second.
	TickRate int
	// Gravity is the downward acceleration in world units per second².
	Gravity float64
	// FlapVelocity is the upward speed a flap sets, in world units per second.
	FlapVelocity float64
	// MaxFallSpeed caps the downward speed.
	MaxFallSpeed float64
	// BirdX is the horizontal position of the bird's centre measured from the
	// left edge of the view.
	BirdX float64
	// BirdWidth and BirdHeight are the diameters of the bird's collision
	// ellipse.
	BirdWidth  float64
	BirdHeight float64
	// PipeWidth is the width of a pipe body.
	PipeWidth float64
	// GapMargin is the minimum distance between a gap and the ceiling or the
	// ground.
	GapMargin float64
	// FirstPipe is the horizontal distance from the bird to the first pipe.
	FirstPipe float64
	// Stages is the difficulty curve, ordered by MinScore.
	Stages []Stage
	// Smooth interpolates speed, gap, spacing and gap shift between stages,
	// so difficulty rises a little with every point instead of jumping at
	// stage boundaries. Stage indices (and themes) still change at MinScore.
	Smooth bool
}

// DefaultParams returns the tuned default parameters.
func DefaultParams() Params {
	return Params{
		TickRate:     60,
		Gravity:      3.1,
		FlapVelocity: 0.92,
		MaxFallSpeed: 1.45,
		BirdX:        0.45,
		BirdWidth:    0.095,
		BirdHeight:   0.075,
		PipeWidth:    0.16,
		GapMargin:    0.08,
		FirstPipe:    1.0,
		Stages:       DefaultStages(),
		Smooth:       true,
	}
}

// DefaultStages returns the default difficulty curve.
func DefaultStages() []Stage {
	return []Stage{
		{MinScore: 0, Speed: 0.50, Gap: 0.36, Spacing: 0.90, GapShift: 0.20},
		{MinScore: 10, Speed: 0.54, Gap: 0.34, Spacing: 0.88, GapShift: 0.24},
		{MinScore: 25, Speed: 0.58, Gap: 0.32, Spacing: 0.86, GapShift: 0.28},
		{MinScore: 50, Speed: 0.62, Gap: 0.30, Spacing: 0.84, GapShift: 0.30},
		{MinScore: 100, Speed: 0.66, Gap: 0.29, Spacing: 0.82, GapShift: 0.32},
		{MinScore: 150, Speed: 0.70, Gap: 0.28, Spacing: 0.80, GapShift: 0.32},
	}
}

// StageIndex returns the index of the stage active at the given score.
func (p Params) StageIndex(score int) int {
	idx := 0
	for i, st := range p.Stages {
		if score >= st.MinScore {
			idx = i
		}
	}
	return idx
}

// StageAt returns the difficulty in effect at the given score. With Smooth,
// values are interpolated towards the next stage; past the last stage they
// stay at its values.
func (p Params) StageAt(score int) Stage {
	i := p.StageIndex(score)
	s := p.Stages[i]
	if !p.Smooth || i+1 >= len(p.Stages) {
		return s
	}
	n := p.Stages[i+1]
	span := n.MinScore - s.MinScore
	if span <= 0 {
		return s
	}
	t := float64(score-s.MinScore) / float64(span)
	l := func(a, b float64) float64 { return a + (b-a)*t }
	return Stage{
		MinScore: s.MinScore,
		Speed:    l(s.Speed, n.Speed),
		Gap:      l(s.Gap, n.Gap),
		Spacing:  l(s.Spacing, n.Spacing),
		GapShift: l(s.GapShift, n.GapShift),
	}
}

// Dt returns the duration of one simulation step in seconds.
func (p Params) Dt() float64 { return 1 / float64(p.TickRate) }

// MaxClimb returns the largest height the bird can gain while travelling the
// given horizontal distance at the given speed. Flapping every v/g seconds is
// the fastest sustained climb and yields v/2 world units per second.
func (p Params) MaxClimb(distance, speed float64) float64 {
	return p.FlapVelocity / 2 * distance / speed
}

// Validate reports whether the parameters describe a playable, fair game.
func (p Params) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

	check(p.TickRate >= 10 && p.TickRate <= 240, "tick rate %d must be between 10 and 240", p.TickRate)
	check(finite(p.Gravity) && p.Gravity > 0 && p.Gravity <= 20, "gravity %g must be in (0, 20]", p.Gravity)
	check(finite(p.FlapVelocity) && p.FlapVelocity > 0 && p.FlapVelocity <= 5, "flap velocity %g must be in (0, 5]", p.FlapVelocity)
	check(finite(p.MaxFallSpeed) && p.MaxFallSpeed > 0 && p.MaxFallSpeed <= 10, "max fall speed %g must be in (0, 10]", p.MaxFallSpeed)
	check(finite(p.BirdWidth) && p.BirdWidth > 0.01 && p.BirdWidth <= 0.3, "bird width %g must be in (0.01, 0.3]", p.BirdWidth)
	check(finite(p.BirdHeight) && p.BirdHeight > 0.01 && p.BirdHeight <= 0.3, "bird height %g must be in (0.01, 0.3]", p.BirdHeight)
	check(finite(p.BirdX) && p.BirdX >= p.BirdWidth && p.BirdX <= 1, "bird x %g must be between the bird width and 1", p.BirdX)
	check(finite(p.PipeWidth) && p.PipeWidth >= 0.05 && p.PipeWidth <= 0.5, "pipe width %g must be in [0.05, 0.5]", p.PipeWidth)
	check(finite(p.GapMargin) && p.GapMargin >= 0 && p.GapMargin <= 0.3, "gap margin %g must be in [0, 0.3]", p.GapMargin)
	check(finite(p.FirstPipe) && p.FirstPipe >= 0.5 && p.FirstPipe <= 5, "first pipe distance %g must be in [0.5, 5]", p.FirstPipe)

	if len(p.Stages) == 0 {
		errs = append(errs, errors.New("at least one stage is required"))
	} else if p.Stages[0].MinScore != 0 {
		errs = append(errs, fmt.Errorf("the first stage must start at score 0, not %d", p.Stages[0].MinScore))
	}
	for i, s := range p.Stages {
		if i > 0 && s.MinScore <= p.Stages[i-1].MinScore {
			errs = append(errs, fmt.Errorf("stage %d: min_score %d must be greater than the previous stage's %d", i, s.MinScore, p.Stages[i-1].MinScore))
		}
		check(finite(s.Speed) && s.Speed >= 0.1 && s.Speed <= 3, "stage %d: speed %g must be in [0.1, 3]", i, s.Speed)
		check(finite(s.Gap) && s.Gap >= p.BirdHeight+0.08, "stage %d: gap %g must be at least the bird height plus 0.08 (%g)", i, s.Gap, p.BirdHeight+0.08)
		check(finite(s.Gap) && s.Gap+2*p.GapMargin <= 0.9, "stage %d: gap %g plus twice the gap margin must not exceed 0.9", i, s.Gap)
		check(finite(s.Spacing) && s.Spacing >= p.PipeWidth+p.BirdWidth+0.2 && s.Spacing <= 5, "stage %d: spacing %g must be in [pipe width + bird width + 0.2, 5]", i, s.Spacing)
		check(finite(s.GapShift) && s.GapShift >= 0 && s.GapShift <= 0.6, "stage %d: gap shift %g must be in [0, 0.6]", i, s.GapShift)
		if finite(s.Speed) && s.Speed > 0 && finite(s.Spacing) {
			// The bird must be able to climb from one gap to the next with
			// room to spare, otherwise the course can become impossible.
			reach := 0.8 * p.MaxClimb(s.Spacing-p.PipeWidth, s.Speed)
			check(s.GapShift <= reach, "stage %d: gap shift %g exceeds what the bird can comfortably climb (%.3f)", i, s.GapShift, reach)
		}
	}
	// With smooth ramps, every interpolated step must be reachable too.
	if len(errs) == 0 && p.Smooth {
		last := p.Stages[len(p.Stages)-1].MinScore
		for score := 0; score <= last; score++ {
			s := p.StageAt(score)
			if reach := 0.8 * p.MaxClimb(s.Spacing-p.PipeWidth, s.Speed); s.GapShift > reach {
				errs = append(errs, fmt.Errorf("at score %d the interpolated gap shift %.3f exceeds what the bird can comfortably climb (%.3f)", score, s.GapShift, reach))
				break
			}
		}
	}
	return errors.Join(errs...)
}
