# Architecture

flapd is one statically linked Go binary. It accepts SSH connections, runs
one isolated game per connection, and keeps scores in a local SQLite file.

```
                 TCP :2222
                    │
        ┌───────────▼────────────┐   reject before any crypto:
        │ admission (sshserver)  │   max connections, per-IP concurrency,
        │                        │   per-IP + global token buckets, bans,
        └───────────┬────────────┘   max concurrent handshakes
                    │
        ┌───────────▼────────────┐   x/crypto/ssh with a 10 s deadline covering
        │ handshake + auth       │   KEX and authentication; publickey (identity =
        └───────────┬────────────┘   HMAC(secret, key)) or keyboard-interactive guest
                    │
        ┌───────────▼────────────┐   exactly one "session" channel; pty-req, env
        │ session policy         │   allowlist, window-change, shell; everything
        └───────────┬────────────┘   else refused; game slot reserved in a registry
                    │
     ┌──────────────▼───────────────┐
     │ Bubble Tea program (per player)                     ┌────────────┐
     │  input ← throttled channel reader (idle tracking)  │  store     │
     │  model = tui.App ── game.World (pure simulation)   │  (SQLite)  │
     │  view  → canvas → ANSI string → cell-diff renderer │◄───────────┤
     │  output → ONLCR + stall-watchdog writer → channel  │ async cmds │
     └──────────────────────────────┘                     └────────────┘
```

## Packages

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/game` | Deterministic physics, pipe course generation, ellipse-vs-rect collision, scoring, difficulty stages, parameter validation (including a reachability check) | nothing |
| `internal/canvas` | Cell grid, half-block pixel bitmap, width-aware safe text, frames, colour quantisation (truecolor/256/16), the ANSI encoder, SVG export for screenshots | go-runewidth |
| `internal/tui` | The Bubble Tea model: state machine, screens, layout, scene rendering, effects, input mapping, asynchronous persistence commands | game, canvas, store (types), security, Bubble Tea |
| `internal/sshserver` | SSH policy, admission control, authentication and identity, session lifecycle, watchdog, host keys, allowlist | tui, store, security, config, x/crypto/ssh |
| `internal/store` | Schema migrations, players, runs, leaderboard queries, retention, integrity checks, online backup | modernc.org/sqlite |
| `internal/security` | Name validation and look-alike keys, log sanitising, token buckets, per-address admission and bans, IP redaction | nothing |
| `internal/config` | TOML loading (strict), defaults, validation with actionable messages | go-toml, game |
| `internal/sdnotify` | `READY=1` / `STOPPING=1` for `Type=notify` | nothing |
| `cmd/flapd` | Subcommands, logging, signals, the retention loop | everything |
| `cmd/flapload` | Load generator | x/crypto/ssh |

The simulation has no clock, I/O or global state. The UI has no networking.
The SSH layer knows nothing about rendering beyond starting a program.

## Connection lifecycle

1. **Admission** (`Server.admit`) runs before any SSH bytes are processed.
   Each check is O(1) behind one mutex, and a rejected connection is simply
   closed:
   - a global connection semaphore (`max_connections`);
   - a per-address entry (IPv4 address or IPv6 /64) with a concurrency
     counter, a token bucket and a ban deadline;
   - a global token bucket;
   - a semaphore for in-progress handshakes (`max_unauthenticated`).

   The per-address table is bounded (`tracked_addresses`). Idle entries are
   swept every minute and whenever the table is full; if it is still full,
   new addresses are refused.
2. **Handshake.** The connection deadline (`handshake_timeout`) covers key
   exchange and authentication together. The public-key callback only checks
   policy (key type and size, allowlist). The identity is computed in
   `VerifiedPublicKeyCallback`, after the client has proven it holds the
   private key, and carried in `Permissions.Extensions`. In `open` mode a
   keyboard-interactive round with zero questions admits guests. Failed
   attempts (other than the client's initial `none` probe) count towards a
   temporary ban.
3. **Channels and requests.** Global requests are all answered with failure.
   One `session` channel is accepted, and any further channel is rejected. A
   request goroutine handles:
   - `pty-req`: TERM sanitised, size clamped to `max_terminal_*`;
   - `env`: only `COLORTERM`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TERM_PROGRAM`,
     with bounded printable values;
   - `window-change`: at most 10 per second, bursts coalesced to the final
     size;
   - `shell`: starts the game;
   - `exec` and `subsystem`: refused with a message;
   - everything else: refused.

   If no shell request arrives within `session_start_timeout`, the
   connection closes.
4. **Session start.** The game requires a PTY (otherwise the client gets an
   explanation and exit status 1). Maintenance mode, the global game limit
   and the per-key limit are checked next. Then the profile is loaded with a
   timeout; if storage is down, play continues and scores are saved later.
5. **Program.** A Bubble Tea program is created with:
   - the session context, cancelled on disconnect, watchdog decisions or hard
     shutdown;
   - a throttled input reader: 1 KiB reads paced by a token bucket, so a
     flooding client is slowed by SSH flow control;
   - an output writer that performs ONLCR and exposes how long a write has
     been blocked;
   - an explicit environment and window size, no signal handler, and a
     truecolor pass-through profile (the model quantises colours itself).
6. **Watchdog** (one goroutine per session, ticking every second):
   - a write blocked longer than `write_timeout` drops the connection (the
     client stopped reading);
   - idle input beyond `idle_timeout`, or the session passing
     `max_session_duration`, sends an in-game notice, then cancels after 5 s
     if the game has not ended.

   Keepalives run per connection; a peer silent for
   `keepalive_interval × (max_missed+1)` is dropped.
7. **End.** After the program exits, `App.SaveUnsaved` stores a run that was
   interrupted by quit, disconnect, idle, time limit or shutdown, or whose
   save never completed. Run IDs make this idempotent. The session then
   prints a farewell line on the normal screen, sends `exit-status`, closes
   the channel and the connection, and releases its slots.

Goroutines per player: connection handler, global-request drain, keepalive,
session, request loop, watchdog, Bubble Tea's event loop (in the session
goroutine), input read loop and renderer ticker, plus at most a few
short-lived commands (frame tick, database calls). Every one of them ends
when the connection closes. The integration tests check that goroutines
return to baseline after many sessions, including abrupt disconnects.

### Why ONLCR matters

Bubble Tea's renderer, when its input is not a TTY, assumes newlines are
translated to CR LF (as a server-side PTY would do) and moves the cursor
down with bare `\n`. flapd deliberately allocates no server PTY, and the
client's terminal is in raw mode, so a bare LF keeps the column. Without
translation every multi-line update would be shifted. The integration test
caught exactly that with a terminal emulator. `outputWriter` therefore
expands LF to CR LF, which is correct for every client, including Windows
consoles that add CR themselves.

### Shutdown

On SIGTERM, flapd:

1. stops accepting connections;
2. sends every game a notice ("The server is restarting. Your score will be
   saved."), and games quit themselves;
3. waits up to `shutdown_grace` for sessions to end;
4. cancels the rest, closes remaining connections, waits for all goroutines,
   then closes the database.

Interrupted runs are saved with reason `shutdown`. systemd's
`TimeoutStopSec` (30 s) covers the grace period.

## Game loop and timing

- The simulation (`game.World.Step`) advances a fixed `1/tick_rate` (60 Hz
  by default). It is pure and seeded, so a run is fully determined by its
  seed and the ticks at which flaps happened. Tests replay the same inputs at
  different view widths and get identical results.
- The UI runs on frame ticks: up to `render_fps` during play (30, reduced
  for very large terminals, minimum 18) and `menu_fps` (12) elsewhere. Each
  tick adds real elapsed time to an accumulator and runs as many fixed steps
  as fit, up to 12. If the server stalls, the backlog is dropped instead of
  fast-forwarding the bird into a pipe.
- A flap is applied the moment the key event arrives and appears on the next
  frame. A flap does not force an immediate re-render.
- The world is normalised: the playfield is 1.0 tall, and the bird, pipes,
  gaps and speeds use the same unit. A resize therefore only changes the
  mapping to cells. The visible width is clamped to 1.35–2.2 units
  (letterboxing beyond that), so look-ahead stays within a fair range on any
  terminal shape.
- Difficulty is a list of anchor stages. With `smooth_difficulty` (the
  default), `Params.StageAt(score)` interpolates speed, gap, spacing and gap
  shift between anchors, so the game gets harder with every point; past the
  last anchor the values stay fixed. The stage index, which drives the sky
  theme, the banner and the bell, still changes only at each anchor's
  `min_score`.
- Gap and spacing for pipe *i* use `StageAt(i)`, so the course depends only
  on the seed. Validation rejects configurations where the next gap could be
  higher than the bird can climb (with a 20% margin). A test shows a simple
  bot with a 120 ms reaction delay (a slow SSH round trip) averages about 80
  points.

## Rendering and bandwidth

Each frame:

1. Copy the cached sky. It is re-rendered only when the size, theme or
   aurora phase changes.
2. Draw clouds, hills, pipes, ground, bird, particles and HUD digits into a
   pixel bitmap (two pixel rows per cell, or one in ASCII mode).
3. Compose the bitmap into cells with `▀` (top pixel as foreground, bottom
   as background); a cell whose two pixels match becomes a plain space.
4. Draw text and cards as cells.
5. Encode to an SGR string. The encoder emits only style changes and gives
   blank cells a canonical (default) foreground, so visually identical cells
   compare equal in the renderer's diff.

Bubble Tea parses the string into its own cell buffer and sends only the
differences, at most at `render_fps`, using cursor movement and erase or
delete-character optimisations.

Bandwidth was measured with the same renderer (`TestBandwidthBudget`).
Moving pipes dominate, because every band boundary shifts each frame. These
choices cut the original cost roughly in half:

- per-cell-row sky gradients, so plain sky is 1-byte spaces rather than
  3-byte half blocks;
- canonical blank cells;
- three-band pipe bodies (outline kept on caps);
- slow, plateaued parallax hills;
- static dirt with scrolling grass stripes;
- frame rate scaled with terminal area for large terminals.

Current numbers in 256-colour mode during active play:

| Size | FPS | Bytes/frame | KB/s |
|---|---|---|---|
| 80×24 | 30 | ~2,800 | ~83 |
| 100×30 | 24 | ~4,100 | ~96 |
| 120×40 | 19 | ~5,700 | ~106 |
| 160×50 | 18 | ~8,000 | ~141 |

Truecolor costs about 70% more because SGR sequences are longer. That is one
reason `xterm-256color` clients default to 256 colours. Menus are mostly
static and cost very little.

CPU per frame (simulate + render + encode, `BenchmarkFrame`) is 39 µs at
50×18 and 244 µs at 160×50. Bubble Tea's parse and diff add a similar amount.

## Sound

Over SSH the server can only send terminal control sequences. It cannot play
audio files. The terminal bell (BEL, `\a`) is the only sound supported
across Windows Terminal, the Windows console, and Linux and macOS terminals.
flapd therefore uses it as its sound effect, rung through `tea.Raw`, which
Bubble Tea writes between frames.

- **When it rings:** stage changes (speed-ups), beating your personal best
  during a run, and crashing. Bells less than 0.35 s apart are merged, and
  nothing rings per flap or per point.
- **Preference:** players toggle it with S. It is stored per player
  (`players.sound_mode`, schema migration 2), and the default comes from
  `ui.default_sound`.
- **DECPS was rejected.** The VT520 sequence for playing notes is
  implemented only by Windows Terminal and Contour. In Windows Terminal it
  also blocks further output until the notes finish
  ([microsoft/terminal#13208](https://github.com/microsoft/terminal/pull/13208)),
  which would freeze the game display.

## Persistence

```sql
players(id, kind['key'|'guest'], identity UNIQUE (64-hex HMAC, NULL for guests),
        name, name_key UNIQUE (look-alike key), created_at, last_seen_at,
        best_score, best_at, total_runs, total_score, color_mode, glyph_mode,
        sound_mode)
runs(id, run_uuid UNIQUE, player_id → players ON DELETE CASCADE, score,
     duration_ms, ticks, flaps, seed, reason, ended_at)
```

- Both tables are STRICT with CHECK constraints. There is a partial index for
  the leaderboard and indexes for history and pruning.
- Pragmas: WAL, `synchronous=FULL`, `foreign_keys=ON`, `trusted_schema=OFF`,
  `cell_size_check=ON`, `busy_timeout=5000`. The file is created `0600`.
- Every write is an IMMEDIATE transaction (no lock-upgrade deadlocks), with a
  short retry on `SQLITE_BUSY`. `RecordRun` inserts the run (`ON CONFLICT
  DO NOTHING` on `run_uuid`), updates the player's totals and best, and
  computes the new rank, all in one transaction.
- Migrations are versioned in `schema_migrations`. A database newer than the
  binary is refused, which protects rollbacks from silent corruption.
- Startup runs `quick_check` (configurable to `full` or `off`) and
  `foreign_key_check`. `flapd check-db` and `flapd backup` (`VACUUM INTO`
  plus verification of the copy) are safe while the service runs.
- Hourly maintenance prunes old runs and idle guests, checkpoints the WAL and
  runs `PRAGMA optimize`.
- Every UI database call runs as a Bubble Tea command, so it never blocks
  rendering, and has a timeout (`operation_timeout`).
- Latency with 5,000 players and 20,000 runs (`BenchmarkRecordRun`,
  `BenchmarkLeaderboardPage`, `BenchmarkRank`): 0.86 ms per saved run
  (including the fsync), 0.33 ms per leaderboard page, 0.73 ms per rank
  lookup.

## Testing

| Area | Where |
|---|---|
| Physics, collisions, course fairness, determinism, difficulty, latency bot | `internal/game` |
| Encoder safety (escape injection, bidi, zero-width, wide runes), quantisation, clipping | `internal/canvas` |
| Names, look-alikes, token buckets, admission, bans, bounded tracking, IP redaction | `internal/security` |
| Config defaults = sample file, strict keys, validation, security controls can't be disabled | `internal/config` |
| Migrations, idempotent atomic writes, ordering and ranks, concurrency (32 writers), retention, backup/restore, corruption detection, schema downgrade refusal | `internal/store` |
| Every screen at 16 sizes × 2 glyph modes (exact frame size, no control characters), resize/too-small/recovery, quit keys, saving/retry/recovery, guest profiles, name editing, hostile stored names, ASCII-only output, colour modes, pause, notices, lag clamping, bandwidth budget | `internal/tui` |
| Real SSH sessions through a terminal emulator: play, resize, exit and terminal restoration, guests, allowlist and bans, refusal of exec/sftp/forwarding/x11/agent/extra channels, no-PTY, session limits, handshake and rate limits, restart persistence, concurrent isolation and goroutine cleanup, abrupt disconnect saving, graceful shutdown, idle timeout, stalled clients, malformed requests, host key and secret persistence, identity keying, weak keys | `internal/sshserver` |
| Install/upgrade/rollback/uninstall/purge under systemd | `deploy/test/systemd-test.sh` |
