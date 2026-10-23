// Package store persists players, runs and the leaderboard in SQLite.
//
// The database runs in WAL mode with synchronous=FULL, so every committed
// score survives process crashes and power loss. All writes happen inside
// IMMEDIATE transactions, which serialises writers without lock-upgrade
// deadlocks, and every statement uses bound parameters.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"flap/internal/security"
)

// Errors returned by the store.
var (
	ErrNotFound     = errors.New("not found")
	ErrNameTaken    = errors.New("that name is already taken")
	ErrInvalidRun   = errors.New("invalid run record")
	ErrSchemaTooNew = errors.New("database schema is newer than this binary")
)

// Kind distinguishes players with a public-key identity from guests.
type Kind string

const (
	KindKey   Kind = "key"
	KindGuest Kind = "guest"
)

// Player is a stored player profile.
type Player struct {
	ID        int64
	Kind      Kind
	Name      string
	CreatedAt time.Time
	Best      int
	BestAt    time.Time
	Runs      int
	ColorMode string
	GlyphMode string
	SoundMode string
}

// End reasons for runs.
const (
	ReasonPipe       = "pipe"
	ReasonGround     = "ground"
	ReasonQuit       = "quit"
	ReasonDisconnect = "disconnect"
	ReasonShutdown   = "shutdown"
	ReasonIdle       = "idle"
	ReasonTimeLimit  = "time_limit"
)

var validReasons = map[string]bool{
	ReasonPipe: true, ReasonGround: true, ReasonQuit: true,
	ReasonDisconnect: true, ReasonShutdown: true, ReasonIdle: true, ReasonTimeLimit: true,
}

// Run is a finished run computed by the server-side simulation.
type Run struct {
	// UUID makes saving idempotent: saving the same run twice records it
	// once. Use NewRunID.
	UUID     string
	Score    int
	Duration time.Duration
	Ticks    int
	Flaps    int
	Seed     uint64
	Reason   string
	EndedAt  time.Time
}

// RunResult describes the effect of recording a run.
type RunResult struct {
	PlayerID  int64
	Best      int
	NewBest   bool
	Rank      int // 1-based leaderboard position of the player's best, 0 if unranked
	Ranked    int // number of ranked players
	Duplicate bool
}

// Entry is one leaderboard row.
type Entry struct {
	Rank     int
	PlayerID int64
	Name     string
	Kind     Kind
	Score    int
	At       time.Time
	Runs     int
}

// Page is a slice of the leaderboard.
type Page struct {
	Entries []Entry
	Total   int
}

// RunSummary is a run from a player's history.
type RunSummary struct {
	Score   int
	Reason  string
	EndedAt time.Time
}

// Options configures Open.
type Options struct {
	// IntegrityCheck is "quick", "full" or "off".
	IntegrityCheck string
	// GuestLeaderboard includes guests in rankings.
	GuestLeaderboard bool
	// Logger receives warnings; nil discards them.
	Logger *slog.Logger
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Store is the persistence layer. It is safe for concurrent use.
type Store struct {
	db     *sql.DB
	guests bool
	now    func() time.Time
	log    *slog.Logger
}

// Open opens (creating if needed) the database at path, verifies its
// integrity and applies pending migrations.
func Open(ctx context.Context, path string, opt Options) (*Store, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("invalid database path %q", path)
	}
	log := opt.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	if err := prepareFile(path, log); err != nil {
		return nil, err
	}
	dsn := path + "?_txlock=immediate" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=trusted_schema(OFF)" +
		"&_pragma=cell_size_check(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(10 * time.Minute)
	s := &Store{db: db, guests: opt.GuestLeaderboard, now: now, log: log}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if opt.IntegrityCheck != "off" {
		if err := s.Check(ctx, opt.IntegrityCheck == "full"); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// prepareFile makes sure the database file exists with owner-only
// permissions before SQLite opens it, independent of the process umask.
func prepareFile(path string, log *slog.Logger) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open database file: %w", err)
	}
	st, err := f.Stat()
	f.Close()
	if err != nil {
		return fmt.Errorf("stat database file: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		log.Warn("database file is accessible by other users; tightening to 0600", "path", path, "mode", st.Mode().Perm().String())
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("restrict database permissions: %w", err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests and maintenance tooling.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("%w: database is at version %d but this binary supports up to %d; "+
			"upgrade flapd or restore the backup taken before the upgrade", ErrSchemaTooNew, current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[v-1]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v, s.now().UnixMilli())
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %d: %w", v, err)
		}
		s.log.Info("applied database migration", "version", v)
	}
	return nil
}

// SchemaVersion returns the applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// Check runs SQLite's integrity checks.
func (s *Store) Check(ctx context.Context, full bool) error {
	pragma := "PRAGMA quick_check"
	if full {
		pragma = "PRAGMA integrity_check"
	}
	rows, err := s.db.QueryContext(ctx, pragma)
	if err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("integrity check: %w", err)
		}
		if line != "ok" && len(problems) < 10 {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("database integrity check failed (restore from a backup, see docs/OPERATIONS.md): %s", strings.Join(problems, "; "))
	}
	fk, err := s.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer fk.Close()
	if fk.Next() {
		return errors.New("database foreign key check failed (restore from a backup, see docs/OPERATIONS.md)")
	}
	return fk.Err()
}

// tx runs fn in an IMMEDIATE transaction, retrying briefly if the database
// is busy.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = s.tryTx(ctx, fn)
		if err == nil || !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(20*(attempt+1)) * time.Millisecond):
		}
	}
	return err
}

func (s *Store) tryTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func isBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		c := se.Code() & 0xff
		return c == sqlite3.SQLITE_BUSY || c == sqlite3.SQLITE_LOCKED
	}
	return false
}

func isUnique(err error, column string) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return column == "" || strings.Contains(se.Error(), column)
	}
	return false
}

// NewRunID returns a random run identifier.
func NewRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

const playerColumns = `id, kind, name, created_at, best_score, best_at, total_runs, color_mode, glyph_mode, sound_mode`

func scanPlayer(row interface{ Scan(...any) error }) (Player, error) {
	var p Player
	var kind string
	var created int64
	var bestAt sql.NullInt64
	if err := row.Scan(&p.ID, &kind, &p.Name, &created, &p.Best, &bestAt, &p.Runs, &p.ColorMode, &p.GlyphMode, &p.SoundMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Player{}, ErrNotFound
		}
		return Player{}, err
	}
	p.Kind = Kind(kind)
	p.CreatedAt = time.UnixMilli(created)
	p.BestAt = fromMS(bestAt)
	return p, nil
}

// KeyPlayer returns the player for a public-key identity, creating it with a
// generated name on first sight. identity must be a 64-character hex digest.
func (s *Store) KeyPlayer(ctx context.Context, identity string, nameFor func(attempt int) string) (Player, error) {
	if len(identity) != 64 {
		return Player{}, errors.New("identity must be a 64-character hex digest")
	}
	var p Player
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := ms(s.now())
		var err error
		p, err = scanPlayer(tx.QueryRowContext(ctx, `SELECT `+playerColumns+` FROM players WHERE identity = ?`, identity))
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE players SET last_seen_at = ? WHERE id = ?`, now, p.ID)
			return err
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		for attempt := 0; attempt < 20; attempt++ {
			name := nameFor(attempt)
			res, err := tx.ExecContext(ctx, `INSERT INTO players (kind, identity, name, name_key, created_at, last_seen_at)
				VALUES ('key', ?, ?, ?, ?, ?) ON CONFLICT (name_key) DO NOTHING`,
				identity, name, security.NameKey(name), now, now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				id, err := res.LastInsertId()
				if err != nil {
					return err
				}
				p = Player{ID: id, Kind: KindKey, Name: name, CreatedAt: time.UnixMilli(now)}
				return nil
			}
		}
		return errors.New("could not allocate a unique player name")
	})
	return p, err
}

// CreateGuest stores a new guest player.
func (s *Store) CreateGuest(ctx context.Context, nameFor func(attempt int) string) (Player, error) {
	var p Player
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := ms(s.now())
		for attempt := 0; attempt < 20; attempt++ {
			name := nameFor(attempt)
			res, err := tx.ExecContext(ctx, `INSERT INTO players (kind, identity, name, name_key, created_at, last_seen_at)
				VALUES ('guest', NULL, ?, ?, ?, ?) ON CONFLICT (name_key) DO NOTHING`,
				name, security.NameKey(name), now, now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				id, err := res.LastInsertId()
				if err != nil {
					return err
				}
				p = Player{ID: id, Kind: KindGuest, Name: name, CreatedAt: time.UnixMilli(now)}
				return nil
			}
		}
		return errors.New("could not allocate a unique guest name")
	})
	return p, err
}

// Player loads a player by ID.
func (s *Store) Player(ctx context.Context, id int64) (Player, error) {
	return scanPlayer(s.db.QueryRowContext(ctx, `SELECT `+playerColumns+` FROM players WHERE id = ?`, id))
}

// Rename changes a key player's display name.
func (s *Store) Rename(ctx context.Context, id int64, name string) error {
	if err := security.ValidateName(name); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE players SET name = ?, name_key = ? WHERE id = ? AND kind = 'key'`,
			name, security.NameKey(name), id)
		if isUnique(err, "name_key") {
			return ErrNameTaken
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Prefs are a player's display and sound preferences.
type Prefs struct {
	ColorMode string
	GlyphMode string
	SoundMode string
}

// SetPrefs stores a player's display and sound preferences.
func (s *Store) SetPrefs(ctx context.Context, id int64, p Prefs) error {
	if len(p.ColorMode) > 16 || len(p.GlyphMode) > 16 || len(p.SoundMode) > 16 {
		return errors.New("preference value too long")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE players SET color_mode = ?, glyph_mode = ?, sound_mode = ? WHERE id = ?`,
			p.ColorMode, p.GlyphMode, p.SoundMode, id)
		return err
	})
}

func (r Run) validate() error {
	switch {
	case len(r.UUID) != 32:
		return fmt.Errorf("%w: bad id", ErrInvalidRun)
	case r.Score < 0 || r.Ticks < 0 || r.Flaps < 0 || r.Duration < 0:
		return fmt.Errorf("%w: negative value", ErrInvalidRun)
	case !validReasons[r.Reason]:
		return fmt.Errorf("%w: unknown reason %q", ErrInvalidRun, r.Reason)
	case r.EndedAt.IsZero():
		return fmt.Errorf("%w: missing end time", ErrInvalidRun)
	}
	return nil
}

// RecordRun atomically stores a run and updates the player's statistics and
// personal best. Recording the same run UUID twice has no further effect.
func (s *Store) RecordRun(ctx context.Context, playerID int64, r Run) (RunResult, error) {
	if err := r.validate(); err != nil {
		return RunResult{}, err
	}
	res := RunResult{PlayerID: playerID}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ins, err := tx.ExecContext(ctx, `INSERT INTO runs (run_uuid, player_id, score, duration_ms, ticks, flaps, seed, reason, ended_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (run_uuid) DO NOTHING`,
			r.UUID, playerID, r.Score, r.Duration.Milliseconds(), r.Ticks, r.Flaps, int64(r.Seed), r.Reason, ms(r.EndedAt))
		if err != nil {
			return err
		}
		n, err := ins.RowsAffected()
		if err != nil {
			return err
		}
		var best int
		var bestAt sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT best_score, best_at FROM players WHERE id = ?`, playerID).Scan(&best, &bestAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if n == 0 {
			res.Duplicate = true
		} else {
			res.NewBest = r.Score > best
			if res.NewBest {
				best, bestAt = r.Score, sql.NullInt64{Int64: ms(r.EndedAt), Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE players SET
					total_runs = total_runs + 1,
					total_score = total_score + ?,
					last_seen_at = ?,
					best_score = ?,
					best_at = ?
				WHERE id = ?`, r.Score, ms(s.now()), best, bestAt, playerID); err != nil {
				return err
			}
		}
		res.Best = best
		kind, err := playerKind(ctx, tx, playerID)
		if err != nil {
			return err
		}
		if best > 0 && (kind == KindKey || s.guests) {
			res.Rank, err = s.rankOf(ctx, tx, playerID, best, bestAt.Int64)
			if err != nil {
				return err
			}
		}
		res.Ranked, err = s.rankedCount(ctx, tx)
		return err
	})
	return res, err
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func playerKind(ctx context.Context, q querier, id int64) (Kind, error) {
	var k string
	err := q.QueryRowContext(ctx, `SELECT kind FROM players WHERE id = ?`, id).Scan(&k)
	return Kind(k), err
}

// rankFilter restricts rankings to key players unless guests are ranked.
func (s *Store) rankFilter() string {
	if s.guests {
		return "best_score > 0"
	}
	return "best_score > 0 AND kind = 'key'"
}

func (s *Store) rankOf(ctx context.Context, q querier, id int64, best int, bestAt int64) (int, error) {
	var ahead int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE `+s.rankFilter()+`
		AND (best_score > ? OR (best_score = ? AND (best_at < ? OR (best_at = ? AND id < ?))))`,
		best, best, bestAt, bestAt, id).Scan(&ahead)
	return ahead + 1, err
}

func (s *Store) rankedCount(ctx context.Context, q querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM players WHERE `+s.rankFilter()).Scan(&n)
	return n, err
}

// Rank returns the player's leaderboard position (0 if unranked) and the
// number of ranked players.
func (s *Store) Rank(ctx context.Context, id int64) (rank, total int, err error) {
	var best int
	var bestAt sql.NullInt64
	var kind string
	if err := s.db.QueryRowContext(ctx, `SELECT best_score, best_at, kind FROM players WHERE id = ?`, id).Scan(&best, &bestAt, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrNotFound
		}
		return 0, 0, err
	}
	total, err = s.rankedCount(ctx, s.db)
	if err != nil || best == 0 || (Kind(kind) == KindGuest && !s.guests) {
		return 0, total, err
	}
	rank, err = s.rankOf(ctx, s.db, id, best, bestAt.Int64)
	return rank, total, err
}

// Leaderboard returns ranked players' best scores, best first. Ties are
// broken by who reached the score first.
func (s *Store) Leaderboard(ctx context.Context, offset, limit int) (Page, error) {
	offset, limit = max(offset, 0), min(max(limit, 0), 1000)
	var page Page
	total, err := s.rankedCount(ctx, s.db)
	if err != nil {
		return page, err
	}
	page.Total = total
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, kind, best_score, best_at, total_runs FROM players
		WHERE `+s.rankFilter()+` ORDER BY best_score DESC, best_at ASC, id ASC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for i := 0; rows.Next(); i++ {
		var e Entry
		var kind string
		var at sql.NullInt64
		if err := rows.Scan(&e.PlayerID, &e.Name, &kind, &e.Score, &at, &e.Runs); err != nil {
			return page, err
		}
		e.Rank = offset + i + 1
		e.Kind = Kind(kind)
		e.At = fromMS(at)
		page.Entries = append(page.Entries, e)
	}
	return page, rows.Err()
}

// RecentRuns returns the player's most recent runs, newest first.
func (s *Store) RecentRuns(ctx context.Context, id int64, limit int) ([]RunSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT score, reason, ended_at FROM runs WHERE player_id = ?
		ORDER BY ended_at DESC, id DESC LIMIT ?`, id, min(max(limit, 0), 100))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var r RunSummary
		var at int64
		if err := rows.Scan(&r.Score, &r.Reason, &at); err != nil {
			return nil, err
		}
		r.EndedAt = time.UnixMilli(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RemovePlayer deletes a player (and, by cascade, their runs) by exact
// display name, for moderation. It reports whether a player was removed.
func (s *Store) RemovePlayer(ctx context.Context, name string) (Player, bool, error) {
	var p Player
	removed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		p, err = scanPlayer(tx.QueryRowContext(ctx, `SELECT `+playerColumns+` FROM players WHERE name = ?`, name))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM players WHERE id = ?`, p.ID); err != nil {
			return err
		}
		removed = true
		return nil
	})
	return p, removed, err
}

// Stats returns row counts for health reporting.
func (s *Store) Stats(ctx context.Context) (players, runs int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM players), (SELECT COUNT(*) FROM runs)`).Scan(&players, &runs)
	return players, runs, err
}

// Retention configures pruning.
type Retention struct {
	RunMaxAge        time.Duration // 0 keeps runs forever
	MaxRunsPerPlayer int
	GuestMaxIdle     time.Duration
	KeepTop          int // players in the top KeepTop are never pruned
}

// PruneResult reports what Prune removed.
type PruneResult struct {
	OldRuns, ExcessRuns, Guests int64
}

// Prune applies the retention policy. Leaderboard bests live on the player
// rows, so deleting run history never changes rankings.
func (s *Store) Prune(ctx context.Context, r Retention) (PruneResult, error) {
	var out PruneResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		if r.RunMaxAge > 0 {
			res, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE ended_at < ?`, ms(now.Add(-r.RunMaxAge)))
			if err != nil {
				return err
			}
			out.OldRuns, _ = res.RowsAffected()
		}
		if r.MaxRunsPerPlayer > 0 {
			res, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE id IN (
				SELECT id FROM (
					SELECT id, ROW_NUMBER() OVER (PARTITION BY player_id ORDER BY ended_at DESC, id DESC) AS rn FROM runs
				) WHERE rn > ?)`, r.MaxRunsPerPlayer)
			if err != nil {
				return err
			}
			out.ExcessRuns, _ = res.RowsAffected()
		}
		if r.GuestMaxIdle > 0 {
			res, err := tx.ExecContext(ctx, `DELETE FROM players WHERE kind = 'guest' AND last_seen_at < ?
				AND id NOT IN (
					SELECT id FROM players WHERE best_score > 0
					ORDER BY best_score DESC, best_at ASC, id ASC LIMIT ?)`,
				ms(now.Add(-r.GuestMaxIdle)), max(r.KeepTop, 0))
			if err != nil {
				return err
			}
			out.Guests, _ = res.RowsAffected()
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	// Keep the WAL from growing without bound and refresh planner stats.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		s.log.Warn("wal checkpoint failed", "err", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		s.log.Warn("optimize failed", "err", err)
	}
	return out, nil
}

// Backup writes a consistent copy of the database to dest using VACUUM INTO.
// It is safe while the service is running. dest must not exist.
func (s *Store) Backup(ctx context.Context, dest string) error {
	if strings.ContainsRune(dest, 0) {
		return errors.New("invalid backup path")
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("backup destination %s already exists", dest)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return os.Chmod(dest, 0o600)
}
