package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct{ ms atomic.Int64 }

func (c *fakeClock) now() time.Time          { return time.UnixMilli(c.ms.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

func openTest(t *testing.T, guests bool) (*Store, string, *fakeClock) {
	t.Helper()
	clk := &fakeClock{}
	clk.ms.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	path := filepath.Join(t.TempDir(), "flap.db")
	s, err := Open(context.Background(), path, Options{IntegrityCheck: "full", GuestLeaderboard: guests, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path, clk
}

func ident(i int) string { return fmt.Sprintf("%064x", i) }

func names(base string) func(int) string {
	return func(a int) string { return fmt.Sprintf("%s%d", base, a) }
}

func run(score int, at time.Time) Run {
	return Run{UUID: NewRunID(), Score: score, Duration: time.Duration(score+1) * time.Second,
		Ticks: (score + 1) * 60, Flaps: score * 3, Seed: uint64(score), Reason: ReasonPipe, EndedAt: at}
}

func TestOpenCreatesPrivateFileAndMigrates(t *testing.T) {
	s, path, _ := openTest(t, true)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("database mode %v, want 0600", st.Mode().Perm())
	}
	v, err := s.SchemaVersion(context.Background())
	if err != nil || v != len(migrations) {
		t.Fatalf("schema version %d (%v), want %d", v, err, len(migrations))
	}
}

func TestKeyPlayerIsStableAndNamesAreUnique(t *testing.T) {
	s, _, _ := openTest(t, true)
	ctx := context.Background()
	a, err := s.KeyPlayer(ctx, ident(1), names("Robin"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.KeyPlayer(ctx, ident(1), names("Other"))
	if err != nil || again.ID != a.ID || again.Name != a.Name {
		t.Fatalf("second lookup returned %+v (%v), want %+v", again, err, a)
	}
	// "Rob1n0" collides with "Robin0" by name key, so the next attempt wins.
	b, err := s.KeyPlayer(ctx, ident(2), func(attempt int) string {
		if attempt == 0 {
			return "R0B1N0"
		}
		return fmt.Sprintf("Robin%d", attempt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "Robin1" {
		t.Fatalf("lookalike name not rejected: %q", b.Name)
	}
	if _, err := s.KeyPlayer(ctx, "short", names("x")); err == nil {
		t.Fatal("bad identity accepted")
	}
}

func TestRecordRunUpdatesBestAtomicallyAndIdempotently(t *testing.T) {
	s, _, clk := openTest(t, true)
	ctx := context.Background()
	p, _ := s.KeyPlayer(ctx, ident(1), names("Ada"))
	r1 := run(5, clk.now())
	res, err := s.RecordRun(ctx, p.ID, r1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NewBest || res.Best != 5 || res.Rank != 1 || res.Ranked != 1 {
		t.Fatalf("first run result %+v", res)
	}
	dup, err := s.RecordRun(ctx, p.ID, r1)
	if err != nil || !dup.Duplicate || dup.NewBest || dup.Best != 5 {
		t.Fatalf("duplicate result %+v (%v)", dup, err)
	}
	clk.advance(time.Minute)
	low, err := s.RecordRun(ctx, p.ID, run(3, clk.now()))
	if err != nil || low.NewBest || low.Best != 5 {
		t.Fatalf("lower run result %+v (%v)", low, err)
	}
	got, _ := s.Player(ctx, p.ID)
	if got.Runs != 2 || got.Best != 5 {
		t.Fatalf("player after runs: %+v", got)
	}
	// A run for an unknown player must not leave a dangling run row.
	if _, err := s.RecordRun(ctx, 999, run(7, clk.now())); !errors.Is(err, ErrNotFound) && err == nil {
		t.Fatalf("unknown player accepted: %v", err)
	}
	_, runs, _ := s.Stats(ctx)
	if runs != 2 {
		t.Fatalf("runs = %d, want 2", runs)
	}
}

func TestRecordRunRejectsInvalidRecords(t *testing.T) {
	s, _, clk := openTest(t, true)
	ctx := context.Background()
	p, _ := s.KeyPlayer(ctx, ident(1), names("Ada"))
	bad := []Run{
		{UUID: "x", Score: 1, Reason: ReasonPipe, EndedAt: clk.now()},
		{UUID: NewRunID(), Score: -1, Reason: ReasonPipe, EndedAt: clk.now()},
		{UUID: NewRunID(), Score: 1, Reason: "teleported", EndedAt: clk.now()},
		{UUID: NewRunID(), Score: 1, Reason: ReasonPipe},
	}
	for i, r := range bad {
		if _, err := s.RecordRun(ctx, p.ID, r); !errors.Is(err, ErrInvalidRun) {
			t.Errorf("case %d: got %v, want ErrInvalidRun", i, err)
		}
	}
}

func TestLeaderboardOrderingAndRanks(t *testing.T) {
	s, _, clk := openTest(t, true)
	ctx := context.Background()
	scores := []int{10, 30, 20, 30, 0}
	var ids []int64
	for i, sc := range scores {
		p, err := s.KeyPlayer(ctx, ident(i+1), names(fmt.Sprintf("P%c", 'a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
		clk.advance(time.Second)
		if _, err := s.RecordRun(ctx, p.ID, run(sc, clk.now())); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.Leaderboard(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Players with a zero best are not ranked; ties go to the earlier score.
	want := []struct {
		id    int64
		score int
	}{{ids[1], 30}, {ids[3], 30}, {ids[2], 20}, {ids[0], 10}}
	if page.Total != 4 || len(page.Entries) != 4 {
		t.Fatalf("page %+v", page)
	}
	for i, w := range want {
		e := page.Entries[i]
		if e.PlayerID != w.id || e.Score != w.score || e.Rank != i+1 {
			t.Fatalf("entry %d = %+v, want id %d score %d", i, e, w.id, w.score)
		}
		rank, total, err := s.Rank(ctx, e.PlayerID)
		if err != nil || rank != i+1 || total != 4 {
			t.Fatalf("Rank(%d) = %d/%d (%v), want %d/4", e.PlayerID, rank, total, err, i+1)
		}
	}
	second, _ := s.Leaderboard(ctx, 2, 10)
	if len(second.Entries) != 2 || second.Entries[0].Rank != 3 {
		t.Fatalf("offset page %+v", second.Entries)
	}
	if r, _, _ := s.Rank(ctx, ids[4]); r != 0 {
		t.Fatalf("zero-score player ranked %d", r)
	}
}

func TestGuestsCanBeExcludedFromRankings(t *testing.T) {
	s, _, clk := openTest(t, false)
	ctx := context.Background()
	g, err := s.CreateGuest(ctx, names("guest-"))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := s.KeyPlayer(ctx, ident(1), names("Keyed"))
	if _, err := s.RecordRun(ctx, g.ID, run(50, clk.now())); err != nil {
		t.Fatal(err)
	}
	res, err := s.RecordRun(ctx, k.ID, run(5, clk.now()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rank != 1 || res.Ranked != 1 {
		t.Fatalf("key player result %+v", res)
	}
	page, _ := s.Leaderboard(ctx, 0, 10)
	if len(page.Entries) != 1 || page.Entries[0].PlayerID != k.ID {
		t.Fatalf("guest leaked onto leaderboard: %+v", page)
	}
	if r, _, _ := s.Rank(ctx, g.ID); r != 0 {
		t.Fatalf("guest ranked %d", r)
	}
}

func TestRename(t *testing.T) {
	s, _, _ := openTest(t, true)
	ctx := context.Background()
	a, _ := s.KeyPlayer(ctx, ident(1), names("Alpha"))
	b, _ := s.KeyPlayer(ctx, ident(2), names("Beta"))
	if err := s.Rename(ctx, a.ID, "Skywalker"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, b.ID, "SKYWA1KER"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("lookalike rename: %v", err)
	}
	if err := s.Rename(ctx, b.ID, "\x1b[31m"); err == nil {
		t.Fatal("unsafe name accepted")
	}
	g, _ := s.CreateGuest(ctx, names("guest-"))
	if err := s.Rename(ctx, g.ID, "GuestRename"); err == nil {
		t.Fatal("guest rename accepted")
	}
	got, _ := s.Player(ctx, a.ID)
	if got.Name != "Skywalker" {
		t.Fatalf("name = %q", got.Name)
	}
	if err := s.SetPrefs(ctx, a.ID, Prefs{ColorMode: "256", GlyphMode: "ascii", SoundMode: "off"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Player(ctx, a.ID)
	if got.ColorMode != "256" || got.GlyphMode != "ascii" || got.SoundMode != "off" {
		t.Fatalf("prefs = %q %q %q", got.ColorMode, got.GlyphMode, got.SoundMode)
	}
}

func TestConcurrentWritesStayConsistent(t *testing.T) {
	s, _, clk := openTest(t, true)
	ctx := context.Background()
	const players, writers, perWriter = 8, 32, 25
	var ids []int64
	for i := 0; i < players; i++ {
		p, err := s.KeyPlayer(ctx, ident(i+1), names(fmt.Sprintf("C%d_", i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	var mu sync.Mutex
	best := map[int64]int{}
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Go(func() {
			for j := 0; j < perWriter; j++ {
				id := ids[(w+j)%players]
				score := (w*31 + j*17) % 97
				r := run(score, clk.now())
				// Every write is attempted twice to exercise idempotency.
				for range 2 {
					if _, err := s.RecordRun(ctx, id, r); err != nil {
						errs <- err
						return
					}
				}
				mu.Lock()
				best[id] = max(best[id], score)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	totalRuns := 0
	for _, id := range ids {
		p, err := s.Player(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Best != best[id] {
			t.Fatalf("player %d best %d, want %d", id, p.Best, best[id])
		}
		totalRuns += p.Runs
	}
	_, runs, _ := s.Stats(ctx)
	if totalRuns != writers*perWriter || runs != writers*perWriter {
		t.Fatalf("runs counted %d, stored %d, want %d", totalRuns, runs, writers*perWriter)
	}
}

func TestPruneRetention(t *testing.T) {
	s, _, clk := openTest(t, true)
	ctx := context.Background()
	k, _ := s.KeyPlayer(ctx, ident(1), names("Keeper"))
	for i := 0; i < 20; i++ {
		if _, err := s.RecordRun(ctx, k.ID, run(i, clk.now())); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Hour)
	}
	topGuest, _ := s.CreateGuest(ctx, names("guest-top"))
	s.RecordRun(ctx, topGuest.ID, run(500, clk.now()))
	idle, _ := s.CreateGuest(ctx, names("guest-idle"))
	s.RecordRun(ctx, idle.ID, run(1, clk.now()))
	clk.advance(40 * 24 * time.Hour)
	res, err := s.Prune(ctx, Retention{RunMaxAge: 0, MaxRunsPerPlayer: 5, GuestMaxIdle: 30 * 24 * time.Hour, KeepTop: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExcessRuns != 15 || res.Guests != 1 {
		t.Fatalf("prune result %+v", res)
	}
	if _, err := s.Player(ctx, idle.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("idle guest kept")
	}
	if _, err := s.Player(ctx, topGuest.ID); err != nil {
		t.Fatal("top guest pruned")
	}
	// Bests survive pruning of the runs that set them.
	if p, _ := s.Player(ctx, k.ID); p.Best != 19 {
		t.Fatalf("best after prune %d", p.Best)
	}
	res, err = s.Prune(ctx, Retention{RunMaxAge: 24 * time.Hour})
	if err != nil || res.OldRuns != 6 {
		t.Fatalf("age prune %+v (%v)", res, err)
	}
}

func TestRestartPreservesScoresAndBackupRestores(t *testing.T) {
	s, path, clk := openTest(t, true)
	ctx := context.Background()
	p, _ := s.KeyPlayer(ctx, ident(9), names("Persist"))
	if _, err := s.RecordRun(ctx, p.ID, run(42, clk.now())); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, backup); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	s.Close()

	for _, file := range []string{path, backup} {
		r, err := Open(ctx, file, Options{IntegrityCheck: "full", GuestLeaderboard: true})
		if err != nil {
			t.Fatal(err)
		}
		again, err := r.KeyPlayer(ctx, ident(9), names("Ignored"))
		if err != nil || again.ID != p.ID || again.Best != 42 {
			t.Fatalf("%s: player %+v (%v)", filepath.Base(file), again, err)
		}
		r.Close()
	}
}

func TestCorruptDatabaseIsRejected(t *testing.T) {
	s, path, clk := openTest(t, true)
	ctx := context.Background()
	p, _ := s.KeyPlayer(ctx, ident(1), names("Victim"))
	for i := 0; i < 200; i++ {
		s.RecordRun(ctx, p.ID, run(i, clk.now()))
	}
	if _, err := s.DB().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 4096*4 {
		t.Fatalf("database unexpectedly small: %d bytes", len(data))
	}
	// Smash the middle of the file (b-tree pages, not the header).
	for i := 4096 * 2; i < 4096*3; i++ {
		data[i] = 0xa5
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, Options{IntegrityCheck: "full"}); err == nil {
		t.Fatal("corrupt database opened without error")
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	s, path, _ := openTest(t, true)
	if _, err := s.DB().Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (99, 0)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(context.Background(), path, Options{IntegrityCheck: "quick"}); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("got %v, want ErrSchemaTooNew", err)
	}
}

func TestCancelledContextFails(t *testing.T) {
	s, _, _ := openTest(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Leaderboard(ctx, 0, 10); err == nil {
		t.Fatal("query with cancelled context succeeded")
	}
	if _, err := s.KeyPlayer(ctx, ident(1), names("X")); err == nil {
		t.Fatal("write with cancelled context succeeded")
	}
}

func TestRecentRuns(t *testing.T) {
	s, _, clk := openTest(t, true)
	ctx := context.Background()
	p, _ := s.KeyPlayer(ctx, ident(1), names("Recent"))
	for i := 1; i <= 4; i++ {
		clk.advance(time.Minute)
		s.RecordRun(ctx, p.ID, run(i, clk.now()))
	}
	got, err := s.RecentRuns(ctx, p.ID, 3)
	if err != nil || len(got) != 3 || got[0].Score != 4 || got[2].Score != 2 {
		t.Fatalf("recent runs %+v (%v)", got, err)
	}
}

func TestRemovePlayer(t *testing.T) {
	s, _, clk := openTest(t, true)
	ctx := context.Background()
	p, _ := s.KeyPlayer(ctx, ident(1), names("Cheater"))
	keep, _ := s.KeyPlayer(ctx, ident(2), names("Honest"))
	s.RecordRun(ctx, p.ID, run(9999, clk.now()))
	s.RecordRun(ctx, keep.ID, run(10, clk.now()))
	got, ok, err := s.RemovePlayer(ctx, p.Name)
	if err != nil || !ok || got.ID != p.ID {
		t.Fatalf("RemovePlayer = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := s.RemovePlayer(ctx, "nobody"); ok {
		t.Fatal("removed a player that does not exist")
	}
	page, _ := s.Leaderboard(ctx, 0, 10)
	if len(page.Entries) != 1 || page.Entries[0].PlayerID != keep.ID {
		t.Fatalf("leaderboard after removal: %+v", page.Entries)
	}
	_, runs, _ := s.Stats(ctx)
	if runs != 1 {
		t.Fatalf("runs not cascaded: %d", runs)
	}
}

// TestMigratesVersionOneDatabase upgrades a database created by the first
// schema version and checks existing data survives.
func TestMigratesVersionOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	ctx := context.Background()
	saved := migrations
	migrations = saved[:1]
	old, err := Open(ctx, path, Options{IntegrityCheck: "full"})
	if err != nil {
		migrations = saved
		t.Fatal(err)
	}
	// Data written by a version-one binary.
	_, err = old.DB().Exec(`INSERT INTO players (id, kind, identity, name, name_key, created_at, last_seen_at, best_score, best_at, total_runs)
		VALUES (7, 'key', ?, 'Veteran', 'veteran', 1, 1, 33, 1, 1)`, ident(1))
	if err != nil {
		migrations = saved
		t.Fatal(err)
	}
	old.Close()
	migrations = saved
	p := Player{ID: 7}

	s, err := Open(ctx, path, Options{IntegrityCheck: "full", GuestLeaderboard: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.SchemaVersion(ctx); v != len(migrations) {
		t.Fatalf("schema version %d, want %d", v, len(migrations))
	}
	got, err := s.Player(ctx, p.ID)
	if err != nil || got.Best != 33 || got.SoundMode != "" {
		t.Fatalf("player after migration: %+v %v", got, err)
	}
	if err := s.SetPrefs(ctx, p.ID, Prefs{SoundMode: "on"}); err != nil {
		t.Fatal(err)
	}
}
