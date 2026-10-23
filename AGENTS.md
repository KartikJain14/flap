# Project state and handoff notes

Read this first if you are picking up the project with no other context.
Keep it current: update the "Status" and "Log" sections whenever you change
something.

## What this is

Flap is a Flappy Bird–style terminal game served over SSH (`ssh -p 2222
flappy@host`). It is a single static Go binary (`flapd`). The pieces:

- its own hardened SSH server (`golang.org/x/crypto/ssh`, with no shell or
  forwarding);
- a Bubble Tea v2 UI on a custom pixel/cell canvas;
- a deterministic simulation;
- SQLite (pure Go) for players and the leaderboard;
- a hardened systemd deployment.

The user's original brief: production-ready, visually polished, adaptive to
terminal size, persistent high scores, secure to expose publicly, and fully
tested and documented.

## Status (2026-10-24)

**v1 is complete; every requirement in the brief is implemented.** All checks
pass:

| Check | Command | Result |
|---|---|---|
| Unit + integration tests | `make test` | pass (104 test functions; real SSH sessions through a VT emulator) |
| Race detector | `make race` | pass |
| vet, staticcheck | `make vet lint` | clean |
| govulncheck | `make vuln` | 0 reachable; only a module note on x/crypto/openpgp, which is not imported |
| shellcheck | `make shellcheck` | clean (needs Docker) |
| systemd deployment | `make deploy-test` | pass on Debian 12 and Ubuntu 24.04 (needs Docker, privileged containers) |
| Real OpenSSH 10.5 client | manual tmux procedure below | pass: play, resize, quit, guest, refusals |
| Load | `bin/flapload` (see README, Performance) | 20 players, 0 failures, no leaks |

**Not done or not verified (open items):**

1. Windows (Win32-OpenSSH, Windows Terminal, conhost), macOS Terminal,
   iTerm2 and PuTTY were not tested; no such machines were available. A
   manual checklist is in `docs/OPERATIONS.md#client-compatibility-checklist`.
2. No real VPS deployment has been done. The user did not authorise one; do
   not deploy without asking.
3. There is no LICENSE file; the user has to choose one.
4. The code is on GitHub at https://github.com/KartikJain14/flap (branch
   `main`). Commit and push only when the user asks.
5. Optional ideas, none requested:
   - dithering for the 256-colour sky (banding is visible);
   - more bandwidth reduction (pipes dominate; see ARCHITECTURE.md,
     "Rendering and bandwidth");
   - an admin command to list or search players.

## Where things are

```
cmd/flapd/            CLI: serve, init, check-config [-listen], keygen, fingerprint, check-db, backup, remove-player, healthcheck, play, version
cmd/flapload/         load generator (-n, -duration, -cycle, -burst, -pid, -hostkey)
internal/game/        pure deterministic simulation (params.go, world.go)
internal/canvas/      cell grid, half-block pixels, sanitising ANSI encoder, colour quantisation, SVG export
internal/tui/         Bubble Tea model (app.go = state machine and commands; screens.go = drawing; scene.go = sky/pipes/ground; sprite.go = procedural bird; layout.go; theme.go; font.go)
internal/sshserver/   server.go (admission, handshake, channels, keepalive, shutdown), session.go (requests, program, watchdog), iowrap.go (input throttle, ONLCR output), keys.go, registry.go
internal/store/       SQLite schema/migrations, RecordRun, leaderboard, retention, backup
internal/security/    names, token bucket, per-IP admission and bans, IP redaction
internal/config/      TOML config + validation; deploy/flapd.toml must equal Default() (a test enforces it)
deploy/               flapd.service, flapd.toml, install.sh, uninstall.sh, rollback.sh, test/ (systemd container test)
docs/                 ARCHITECTURE.md, SECURITY.md (threat model), OPERATIONS.md (runbook), screenshots/
```

## Commands

```sh
make build            # bin/flapd + bin/flapload (static, CGO_ENABLED=0, -trimpath); go.mod pins toolchain go1.27.3
make check            # vet + staticcheck + tests + race + govulncheck
make deploy-test      # Docker systemd containers; takes ~2 min per distro
make shots            # regenerate docs/screenshots (ImageMagick)
./bin/flapd play      # play locally without SSH
```

Manual end-to-end with the real OpenSSH client. Use a separate tmux socket so
the user's own tmux sessions are not touched:

```sh
sed -e 's|0.0.0.0:2222|127.0.0.1:2299|' -e "s|/var/lib/flapd|$PWD/dev-state|g" deploy/flapd.toml > /tmp/dev.toml
./bin/flapd serve -config /tmp/dev.toml &
tmux -L flaptest new-session -d -s g -x 100 -y 30 'ssh -p 2299 -o StrictHostKeyChecking=accept-new flappy@127.0.0.1'
tmux -L flaptest send-keys -t g Space; tmux -L flaptest capture-pane -t g -p
tmux -L flaptest resize-window -t g -x 70 -y 22     # sends a real window-change
tmux -L flaptest kill-server
```

## Decisions and gotchas (read before changing code)

- **ONLCR in `sshserver/iowrap.go` is essential.** Bubble Tea assumes `\n`
  returns to column 0 when its input is not a TTY. flapd has no server PTY
  and the client is in raw mode, so the output writer expands LF to CR LF.
  Removing it garbles every frame; the integration tests catch this.
- **Identity** is `HMAC(identity_secret, pubkey)`, set in
  `VerifiedPublicKeyCallback` (after proof of possession). The SSH user name
  is never trusted. Guests (keyboard-interactive, `open` mode only) get a
  profile row only when they first save a run (`Session.Resolve`, memoised).
- **Never wait on the peer during session teardown.** x/crypto closes a
  channel's request stream only when the peer acknowledges or the connection
  dies, so the session closes the connection itself.
- **Bandwidth is the main runtime cost.** `internal/tui/bandwidth_test.go`
  enforces budgets in 256-colour mode (~83–141 KB/s by size). The encoder
  gives blank cells a canonical default foreground on purpose, so identical
  cells diff as equal. The sky gradient steps per cell row. Pipe bodies have
  three bands. `PlayFPS` lowers FPS for large terminals.
- **The simulation is normalised** (playfield height = 1.0). Resizing never
  changes game state; it pauses the run. The visible width is clamped to
  1.35–2.2 units. `game.Params.Validate` rejects unreachable gaps.
- **The UI never blocks on the database.** All store calls are tea.Cmds with
  timeouts. `App.SaveUnsaved` is called by the session after the program
  exits (quit, disconnect, shutdown, idle); run UUIDs make it idempotent.
- **Load testing from one machine** needs the per-IP limits raised
  (`max_connections_per_ip`, `connection_rate_per_ip`), otherwise most
  connections are refused (correctly).
- **Tooling quirk:** the file-writing tool may turn `\uXXXX` escapes in Go
  source into literal invisible characters, and Go rejects a literal BOM.
  Use numeric code points, or check with
  `grep -nP '[\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2066}-\x{2069}\x{feff}]'`.
- `-race` needs cgo; the Makefile sets `CGO_ENABLED=1` only for `make race`.
- x/vt's `Emulator.Close` races with `Read`; the test harness closes the
  reply pipe instead (see `harness_test.go`).

## Measured numbers (for reports)

Measured on a Ryzen 5 5600X, static binary, 256 colours:

- **Idle:** 0% CPU, 17 MB RSS.
- **20 players at 100×30:** 32–42% of one core, about 80 MB RSS, about
  54 KB/s per player on average, connect p50 37 ms.
- **20 players at 160×50:** 48–63% of one core, about 150 MB RSS, about
  85 KB/s per player.
- **Churn** (15 players reconnecting every 2 s for 30 s): 221 sessions,
  0 failures, flat RSS, fds back to baseline.
- **Burst** of 300 connections: 17 games, 283 refused, no growth.
- **Per frame:** 39 µs at 50×18 to 244 µs at 160×50.
- **Database** (5,000 players, 20,000 runs): RecordRun 0.86 ms, leaderboard
  page 0.33 ms, rank 0.73 ms.
- **Binary:** 12 MB static. `systemd-analyze security` exposure: 1.2 (OK).

## Log

- 2026-10-24: Built v1 end to end: game, TUI, SSH server, store, config,
  systemd deployment, docs and tests. Fixed issues found during
  verification:
  - ONLCR, found with the emulator;
  - bandwidth roughly halved;
  - session teardown no longer waits on the peer;
  - pinned toolchain go1.27.3 for a crypto/x509 stdlib advisory.

  Added `remove-player`, store latency benchmarks, and this file.
- 2026-10-24: Initialised git and pushed the first commit to
  `origin/main` (github.com/KartikJain14/flap), at the user's request.
