package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
)

var ErrNoWings = errors.New("not enough wings")

// ContinueCost doubles after each purchase in a run. The cap avoids overflow.
func ContinueCost(retry int) int {
	if retry < 0 || retry > 30 {
		return 0
	}
	return 1 << retry
}

// SpendWings atomically purchases a continue. The run/retry pair makes a
// repeated request safe even if the caller lost the first successful reply.
func (s *Store) SpendWings(ctx context.Context, playerID int64, runID string, retry int) (int, error) {
	cost := ContinueCost(retry)
	if b, err := hex.DecodeString(runID); err != nil || len(b) != 16 || cost == 0 {
		return 0, ErrInvalidRun
	}
	var balance int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT wings FROM players WHERE id = ?`, playerID).Scan(&balance); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var owner int64
		err := tx.QueryRowContext(ctx, `SELECT player_id FROM continues WHERE run_uuid = ? AND retry = ?`, runID, retry).Scan(&owner)
		if err == nil {
			if owner != playerID {
				return ErrInvalidRun
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var purchases, finished int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM continues WHERE run_uuid = ?`, runID).Scan(&purchases); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE run_uuid = ?`, runID).Scan(&finished); err != nil {
			return err
		}
		if purchases != retry || finished != 0 {
			return ErrInvalidRun
		}
		if retry > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT player_id FROM continues WHERE run_uuid = ? AND retry = 0`, runID).Scan(&owner); err != nil {
				return err
			}
			if owner != playerID {
				return ErrInvalidRun
			}
		}
		if balance < cost {
			return ErrNoWings
		}
		if _, err := tx.ExecContext(ctx, `UPDATE players SET wings = wings - ? WHERE id = ?`, cost, playerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO continues(run_uuid, retry, player_id) VALUES (?, ?, ?)`, runID, retry, playerID); err != nil {
			return err
		}
		balance -= cost
		return nil
	})
	return balance, err
}

// CollectWings credits actual server-observed pickups immediately. Receipts
// also let RecordRun recover failed writes without awarding the same wing twice.
func (s *Store) CollectWings(ctx context.Context, playerID int64, runID string, pipes []int) (int, error) {
	if b, err := hex.DecodeString(runID); err != nil || len(b) != 16 {
		return 0, ErrInvalidRun
	}
	for i, p := range pipes {
		if p < 0 || (i > 0 && p <= pipes[i-1]) {
			return 0, ErrInvalidRun
		}
	}
	var balance int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := creditPickups(ctx, tx, playerID, runID, pipes); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT wings FROM players WHERE id = ?`, playerID).Scan(&balance)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return balance, err
}

func creditPickups(ctx context.Context, tx *sql.Tx, playerID int64, runID string, pipes []int) (int, error) {
	earned := 0
	if len(pipes) > 0 {
		var owner int64
		err := tx.QueryRowContext(ctx, `SELECT player_id FROM wing_pickups WHERE run_uuid=?
            UNION ALL SELECT player_id FROM continues WHERE run_uuid=?
            UNION ALL SELECT player_id FROM runs WHERE run_uuid=? LIMIT 1`, runID, runID, runID).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil && owner != playerID {
			return 0, ErrInvalidRun
		}
	}
	for _, p := range pipes {
		res, err := tx.ExecContext(ctx, `INSERT INTO wing_pickups(run_uuid,pipe_index,player_id) VALUES (?,?,?) ON CONFLICT DO NOTHING`, runID, p, playerID)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			var owner int64
			if err := tx.QueryRowContext(ctx, `SELECT player_id FROM wing_pickups WHERE run_uuid=? AND pipe_index=?`, runID, p).Scan(&owner); err != nil {
				return 0, err
			}
			if owner != playerID {
				return 0, ErrInvalidRun
			}
		}
		earned += int(n)
	}
	if earned > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE players SET wings=wings+? WHERE id=?`, earned, playerID); err != nil {
			return 0, err
		}
	}
	return earned, nil
}
