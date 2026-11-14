package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWingsRewardsAndSpending(t *testing.T) {
	s, _, _ := openTest(t, true)
	ctx := context.Background()
	p, err := s.KeyPlayer(ctx, ident(91), names("WingPilot"))
	if err != nil || p.Wings != 3 {
		t.Fatalf("starter wings: %+v %v", p, err)
	}
	id := NewRunID()
	for retry, want := range []int{2, 0} {
		for repeat := 0; repeat < 2; repeat++ {
			got, err := s.SpendWings(ctx, p.ID, id, retry)
			if err != nil || got != want {
				t.Fatalf("retry %d repeat %d: %d %v", retry, repeat, got, err)
			}
		}
	}
	if _, err := s.SpendWings(ctx, p.ID, id, 2); !errors.Is(err, ErrNoWings) {
		t.Fatalf("overspend: %v", err)
	}
	if _, err := s.SpendWings(ctx, p.ID, NewRunID(), 1); !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("skipped retry: %v", err)
	}
	// Scores alone earn nothing; final saves recover only actual pickups.
	for i, score := range []int{14, 13, 49} {
		r := Run{UUID: NewRunID(), Score: score, Ticks: 1000, Duration: time.Second, Reason: ReasonPipe, EndedAt: time.Now()}
		if i == 2 {
			r.Pickups = []int{24, 49}
		}
		res, err := s.RecordRun(ctx, p.ID, r)
		if err != nil {
			t.Fatal(err)
		}
		want := []int{0, 0, 2}[i]
		if res.Wings != want {
			t.Fatalf("reward: %+v", res)
		}
		again, err := s.RecordRun(ctx, p.ID, r)
		if err != nil || !again.Duplicate || again.Wings != want || again.WingsEarned != 0 {
			t.Fatalf("duplicate reward: %+v %v", again, err)
		}
	}
	p, err = s.Player(ctx, p.ID)
	if err != nil || p.Wings != 2 {
		t.Fatalf("persisted wings: %+v %v", p, err)
	}
	guest, err := s.CreateGuest(ctx, names("guest-wings"))
	if err != nil || guest.Wings != 3 {
		t.Fatalf("guest: %+v %v", guest, err)
	}
	if _, err := s.SpendWings(ctx, guest.ID, id, 0); !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("purchase owned by another player: %v", err)
	}
}

func TestConcurrentWingPurchases(t *testing.T) {
	s, _, _ := openTest(t, true)
	ctx := context.Background()
	p, err := s.KeyPlayer(ctx, ident(92), names("ConcurrentWings"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	runID := NewRunID()
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.SpendWings(ctx, p.ID, runID, 0); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err = s.Player(ctx, p.ID)
	if err != nil || p.Wings != 2 {
		t.Fatalf("charged more than once: %+v %v", p, err)
	}
	// Distinct sessions compete for the same remaining balance.
	successes := make(chan bool, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.SpendWings(ctx, p.ID, NewRunID(), 0); successes <- err == nil }()
	}
	wg.Wait()
	close(successes)
	n := 0
	for ok := range successes {
		if ok {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("successful spends = %d, want 2", n)
	}
}

func TestCollectedWingsCreditOnceAndRecoverOnSave(t *testing.T) {
	s, _, _ := openTest(t, true)
	ctx := context.Background()
	p, err := s.KeyPlayer(ctx, ident(93), names("Collector"))
	if err != nil {
		t.Fatal(err)
	}
	id := NewRunID()
	for i := 0; i < 2; i++ {
		balance, err := s.CollectWings(ctx, p.ID, id, []int{24})
		if err != nil || balance != 4 {
			t.Fatalf("collect %d: %d %v", i, balance, err)
		}
	}
	if balance, err := s.SpendWings(ctx, p.ID, id, 0); err != nil || balance != 3 {
		t.Fatalf("spend newly collected wing: %d %v", balance, err)
	}
	// The second pickup failed to sync before disconnect; final save recovers
	// it without re-awarding the first pickup, even after it was spent.
	r := Run{UUID: id, Score: 50, Ticks: 1000, Duration: time.Second, Reason: ReasonDisconnect, EndedAt: time.Now(), Pickups: []int{24, 49}}
	res, err := s.RecordRun(ctx, p.ID, r)
	if err != nil || res.Wings != 4 || res.WingsEarned != 1 {
		t.Fatalf("recover: %+v %v", res, err)
	}
	for i := 0; i < 2; i++ {
		if balance, err := s.CollectWings(ctx, p.ID, id, []int{24, 49}); err != nil || balance != 4 {
			t.Fatalf("late sync: %d %v", balance, err)
		}
		res, err = s.RecordRun(ctx, p.ID, r)
		if err != nil || res.Wings != 4 || res.WingsEarned != 0 {
			t.Fatalf("late save: %+v %v", res, err)
		}
	}
	other, err := s.CreateGuest(ctx, names("guest-other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CollectWings(ctx, other.ID, id, []int{74}); !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("cross-player pickup: %v", err)
	}
	if _, err := s.CollectWings(ctx, p.ID, NewRunID(), []int{-1}); !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("invalid pipe accepted: %v", err)
	}
}

func TestConcurrentCollectedWingCredit(t *testing.T) {
	s, _, _ := openTest(t, true)
	ctx := context.Background()
	p, err := s.KeyPlayer(ctx, ident(94), names("SharedCollector"))
	if err != nil {
		t.Fatal(err)
	}
	id := NewRunID()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.CollectWings(ctx, p.ID, id, []int{24}); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err = s.Player(ctx, p.ID)
	if err != nil || p.Wings != 4 {
		t.Fatalf("duplicate credit: %+v %v", p, err)
	}
}

func TestPickupMigrationPreservesExistingBalance(t *testing.T) {
	// Build an actual v3 database, including a previously spent starter wing.
	path := t.TempDir() + "/v3.db"
	ctx := context.Background()
	saved := migrations
	migrations = saved[:3]
	old, err := Open(ctx, path, Options{IntegrityCheck: "full"})
	migrations = saved
	if err != nil {
		t.Fatal(err)
	}
	p, err := old.KeyPlayer(ctx, ident(95), names("VeteranCollector"))
	if err != nil {
		old.Close()
		t.Fatal(err)
	}
	if _, err := old.SpendWings(ctx, p.ID, NewRunID(), 0); err != nil {
		old.Close()
		t.Fatal(err)
	}
	old.Close()
	st, err := Open(ctx, path, Options{IntegrityCheck: "full"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p, err = st.Player(ctx, p.ID)
	if err != nil || p.Wings != 2 {
		t.Fatalf("migration reset balance: %+v %v", p, err)
	}
	if balance, err := st.CollectWings(ctx, p.ID, NewRunID(), []int{24}); err != nil || balance != 3 {
		t.Fatalf("new pickup ledger: %d %v", balance, err)
	}
}

func TestPlaytestIntervalMigrationPreservesPickups(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/v4.db"
	saved := migrations
	migrations = saved[:4]
	old, err := Open(ctx, path, Options{IntegrityCheck: "full"})
	migrations = saved
	if err != nil {
		t.Fatal(err)
	}
	p, err := old.KeyPlayer(ctx, ident(96), names("AngelCollector"))
	if err != nil {
		old.Close()
		t.Fatal(err)
	}
	runID := NewRunID()
	if balance, err := old.CollectWings(ctx, p.ID, runID, []int{24}); err != nil || balance != 4 {
		old.Close()
		t.Fatalf("v4 pickup: %d %v", balance, err)
	}
	old.Close()
	st, err := Open(ctx, path, Options{IntegrityCheck: "full"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if balance, err := st.CollectWings(ctx, p.ID, runID, []int{24}); err != nil || balance != 4 {
		t.Fatalf("migration lost receipt: %d %v", balance, err)
	}
	if balance, err := st.CollectWings(ctx, p.ID, runID, []int{4}); err != nil || balance != 5 {
		t.Fatalf("pipe 5 pickup: %d %v", balance, err)
	}
	// Old and testing positions both remain valid when replaying final saves.
	r := Run{UUID: runID, Score: 25, Ticks: 1000, Duration: time.Second, Reason: ReasonPipe, EndedAt: time.Now(), Pickups: []int{4, 24}}
	res, err := st.RecordRun(ctx, p.ID, r)
	if err != nil || res.Wings != 5 || res.WingsEarned != 0 {
		t.Fatalf("mixed pickup reconciliation: %+v %v", res, err)
	}
}
