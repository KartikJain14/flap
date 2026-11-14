package game

import "testing"

func TestRevivePreservesCourseAndProtects(t *testing.T) {
	w := NewWorld(DefaultParams(), 42, 2)
	if w.Revive() {
		t.Fatal("revived a live bird")
	}
	for w.Phase() != Over {
		w.Step()
		w.Events()
	}
	camera, score, ticks, seed := w.Camera(), w.Score(), w.Ticks(), w.Seed()
	pipes := append([]Pipe(nil), w.Pipes()...)
	if !w.Revive() || w.Phase() != Playing || w.Crash() != CrashNone {
		t.Fatal("revive failed")
	}
	if w.Camera() != camera || w.Score() != score || w.Ticks() != ticks || w.Seed() != seed {
		t.Fatal("revive reset run")
	}
	for i, p := range pipes {
		if w.Pipes()[i] != p {
			t.Fatal("revive changed course")
		}
	}
	if w.Invincible() < 3 {
		t.Fatal("missing shield")
	}
	// No input: protection must cover both pipes and the ground.
	for w.Invincible() > 0 {
		w.Step()
		if w.Phase() != Playing {
			t.Fatal("crashed while protected")
		}
		for _, e := range w.Events() {
			if e.Kind == EventCrash {
				t.Fatal("crash effect during protection")
			}
		}
	}
	for i := 0; i < 1000 && w.Phase() == Playing; i++ {
		w.Step()
	}
	if w.Phase() == Playing {
		t.Fatal("shield never expired")
	}
}

func TestWingPickupSpacing(t *testing.T) {
	w := NewWorld(DefaultParams(), 42, 100)
	for _, p := range w.Pipes() {
		if p.HasWing() != ((p.Index+1)%15 == 0) {
			t.Fatalf("pickup at wrong pipe %d", p.Index+1)
		}
	}
	if len(w.Pipes()) < 75 {
		t.Fatal("test did not materialize enough pipes")
	}
}

func TestWingPickupRequiresContactAndIsNotRepeated(t *testing.T) {
	for _, hit := range []bool{false, true} {
		w := NewWorld(DefaultParams(), 42, 2)
		index := WingInterval - 1
		w.camera, w.score, w.nextIndex, w.nextX = 10, index, index+1, 100
		w.pipes = []Pipe{{Index: index, X: w.BirdWorldX() - w.params.PipeWidth/2, GapY: 0.5, Gap: 0.45}}
		w.bird.Y = 0.68
		if hit {
			w.bird.Y = 0.5
		}
		w.Step()
		got := len(w.CollectedWings())
		if (got == 1) != hit {
			t.Fatalf("contact %v collected %d", hit, got)
		}
		count := 0
		for _, e := range w.Events() {
			if e.Kind == EventWing {
				count++
			}
		}
		if count != got {
			t.Fatal("missing collection event")
		}
		if !hit {
			for i := 0; i < 10 && w.Phase() == Playing; i++ {
				w.Step()
			}
			if len(w.CollectedWings()) != 0 {
				t.Fatal("passing pipe awarded wing without contact")
			}
		} else {
			snapshot := w.CollectedWings()
			snapshot[0] = 999
			for w.Phase() != Over {
				w.Step()
			}
			if !w.Revive() {
				t.Fatal("revive failed")
			}
			for i := 0; i < 10; i++ {
				w.Step()
			}
			if got := w.CollectedWings(); len(got) != 1 || got[0] != index {
				t.Fatal("revive or snapshot duplicated/changed pickup")
			}
		}
	}
}
