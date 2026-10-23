package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// populated returns a store with many players and runs, to measure query
// latency at a realistic size.
func populated(b *testing.B, players, runsPer int) (*Store, []int64) {
	b.Helper()
	s, err := Open(context.Background(), filepath.Join(b.TempDir(), "bench.db"), Options{IntegrityCheck: "off", GuestLeaderboard: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	ctx := context.Background()
	ids := make([]int64, 0, players)
	at := time.Unix(1700000000, 0)
	for i := 0; i < players; i++ {
		p, err := s.KeyPlayer(ctx, fmt.Sprintf("%064x", i+1), func(a int) string { return fmt.Sprintf("P%d_%d", i, a) })
		if err != nil {
			b.Fatal(err)
		}
		ids = append(ids, p.ID)
		for j := 0; j < runsPer; j++ {
			at = at.Add(time.Second)
			if _, err := s.RecordRun(ctx, p.ID, Run{UUID: NewRunID(), Score: (i*7 + j*13) % 250, Ticks: 6000, Reason: ReasonPipe, EndedAt: at}); err != nil {
				b.Fatal(err)
			}
		}
	}
	return s, ids
}

func BenchmarkRecordRun(b *testing.B) {
	s, ids := populated(b, 5000, 4)
	ctx := context.Background()
	at := time.Unix(1800000000, 0)
	i := 0
	for b.Loop() {
		i++
		if _, err := s.RecordRun(ctx, ids[i%len(ids)], Run{UUID: NewRunID(), Score: i % 300, Ticks: 6000, Reason: ReasonPipe, EndedAt: at.Add(time.Duration(i) * time.Second)}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLeaderboardPage(b *testing.B) {
	s, _ := populated(b, 5000, 4)
	ctx := context.Background()
	for b.Loop() {
		if _, err := s.Leaderboard(ctx, 0, 20); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRank(b *testing.B) {
	s, ids := populated(b, 5000, 4)
	ctx := context.Background()
	i := 0
	for b.Loop() {
		i++
		if _, _, err := s.Rank(ctx, ids[i%len(ids)]); err != nil {
			b.Fatal(err)
		}
	}
}
