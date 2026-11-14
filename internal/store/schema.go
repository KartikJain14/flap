package store

// migrations are applied in order; migration i brings the schema to version
// i+1. Never edit a released migration: append a new one instead.
var migrations = []string{
	// 1: players, runs and the leaderboard index.
	`
CREATE TABLE players (
	id           INTEGER PRIMARY KEY,
	kind         TEXT    NOT NULL CHECK (kind IN ('key', 'guest')),
	identity     TEXT    UNIQUE CHECK (identity IS NULL OR length(identity) = 64),
	name         TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 32),
	name_key     TEXT    NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	best_score   INTEGER NOT NULL DEFAULT 0 CHECK (best_score >= 0),
	best_at      INTEGER,
	total_runs   INTEGER NOT NULL DEFAULT 0 CHECK (total_runs >= 0),
	total_score  INTEGER NOT NULL DEFAULT 0 CHECK (total_score >= 0),
	color_mode   TEXT    NOT NULL DEFAULT '',
	glyph_mode   TEXT    NOT NULL DEFAULT '',
	CHECK ((kind = 'key') = (identity IS NOT NULL))
) STRICT;

CREATE INDEX players_leaderboard
	ON players (best_score DESC, best_at ASC, id ASC)
	WHERE best_score > 0;

CREATE TABLE runs (
	id          INTEGER PRIMARY KEY,
	run_uuid    TEXT    NOT NULL UNIQUE CHECK (length(run_uuid) = 32),
	player_id   INTEGER NOT NULL REFERENCES players (id) ON DELETE CASCADE,
	score       INTEGER NOT NULL CHECK (score >= 0),
	duration_ms INTEGER NOT NULL CHECK (duration_ms >= 0),
	ticks       INTEGER NOT NULL CHECK (ticks >= 0),
	flaps       INTEGER NOT NULL CHECK (flaps >= 0),
	seed        INTEGER NOT NULL,
	reason      TEXT    NOT NULL,
	ended_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX runs_player ON runs (player_id, ended_at DESC);
CREATE INDEX runs_ended ON runs (ended_at);
`,
	// 2: per-player sound preference.
	`ALTER TABLE players ADD COLUMN sound_mode TEXT NOT NULL DEFAULT '';`,
	// 3: persistent wings and idempotent continue purchases.
	`ALTER TABLE players ADD COLUMN wings INTEGER NOT NULL DEFAULT 3 CHECK (wings >= 0);
CREATE TABLE continues (
    run_uuid TEXT NOT NULL CHECK (length(run_uuid) = 32),
    retry INTEGER NOT NULL CHECK (retry BETWEEN 0 AND 30),
    player_id INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    PRIMARY KEY (run_uuid, retry)
) STRICT;
CREATE INDEX continues_player ON continues(player_id);`,
	// 4: collected wing receipts replace automatic score-based rewards.
	`CREATE TABLE wing_pickups (
 run_uuid TEXT NOT NULL CHECK (length(run_uuid) = 32),
 pipe_index INTEGER NOT NULL CHECK (pipe_index >= 24 AND (pipe_index+1) % 25 = 0),
 player_id INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,
 PRIMARY KEY (run_uuid, pipe_index)
 ) STRICT;
 CREATE INDEX wing_pickups_player ON wing_pickups(player_id);`,
	// 5: pickup positions are defined by the simulation, allowing playtest
	// intervals without invalidating previously collected wings.
	`ALTER TABLE wing_pickups RENAME TO wing_pickups_old;
 DROP INDEX wing_pickups_player;
 CREATE TABLE wing_pickups (
 run_uuid TEXT NOT NULL CHECK (length(run_uuid) = 32),
 pipe_index INTEGER NOT NULL CHECK (pipe_index >= 0),
 player_id INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,
 PRIMARY KEY (run_uuid, pipe_index)
 ) STRICT;
 INSERT INTO wing_pickups SELECT * FROM wing_pickups_old;
 DROP TABLE wing_pickups_old;
 CREATE INDEX wing_pickups_player ON wing_pickups(player_id);`,
}
