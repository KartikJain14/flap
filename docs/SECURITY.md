# Security model

flapd is meant to be exposed to the whole Internet. This document describes
what it protects, what it assumes, how each control works and, just as
importantly, what it does not protect against.

## Assets

1. **The host.** The VPS, other services on it, and the administrator's
   ability to log in.
2. **Availability** of the game for honest players.
3. **Integrity of the leaderboard.** Scores reflect play that actually
   happened on the server.
4. **Player privacy.** The public keys, network addresses and habits of
   players.
5. **Server identity.** The host key that players pin.
6. **Players' terminals,** which must not be abused through what flapd
   prints.

## Trust boundaries and adversaries

| Adversary | Capabilities assumed |
|---|---|
| Internet scanner / botnet | Any number of TCP connections from many addresses, malformed SSH traffic, credential stuffing |
| Malicious player | A valid SSH client with any keys, arbitrary bytes on the channel, odd PTY sizes, request floods, automation (bots) |
| Curious player | Wants to see other players' data or the server's internals |
| Someone holding a copy of the database | For example from a leaked backup |

The operator (root on the VPS) is trusted. The SSH client software is not.

## Controls

### No shell, no commands, no forwarding

flapd is not OpenSSH and has no code path that executes programs. The SSH
layer (`internal/sshserver`) is a whitelist:

- **Channels:** only the first `session` channel per connection is accepted.
  `direct-tcpip`, `forwarded-tcpip`, `x11`, `auth-agent@openssh.com` and any
  further `session` channel are rejected.
- **Session requests:**
  - `pty-req`, `window-change` and `shell` are accepted; `shell` starts the
    game, not a shell;
  - `env` is accepted for five harmless variables only;
  - `exec` and `subsystem` (SFTP, SCP) are refused with an explanation;
  - `x11-req`, `auth-agent-req@openssh.com`, `signal`, `break` and unknown
    requests are refused.
- **Global requests** such as `tcpip-forward` and `cancel-tcpip-forward` are
  all refused.
- **Environment variables** never reach a process environment. They only
  influence colour detection.

The integration tests and a manual run with OpenSSH 10.5 verified each
refusal: `ssh host cmd`, `ssh -T`, `-L`, `-R`, `-A`, `-X`, `scp`, `sftp`.

### Authentication and identity

| Mode | Who gets in | Identity |
|---|---|---|
| `open` (default) | Any SSH key; clients with no acceptable key play as guests via a zero-question keyboard-interactive round | Key players: persistent. Guests: a new random guest name per connection |
| `publickey` | Any SSH key | Persistent |
| `allowlist` | Keys in `auth.authorized_keys_file` (reloaded on SIGHUP; `command=` style options are rejected) | Persistent |

- Password authentication is never offered, in any mode.
- The SSH user name is ignored (only logged, sanitised). It is never used as
  an identity.
- The identity is derived only after the client proves possession of the
  private key (`VerifiedPublicKeyCallback`). That avoids the
  `PublicKeyCallback` misuse class behind CVE-2024-45337.
- What is stored is `HMAC-SHA256(identity_secret, "flap-player-v1" ||
  public key)`. The 32-byte secret is generated on first start (mode 0600).
  Without it, a copy of the database cannot be matched against public key
  directories such as `github.com/<user>.keys`. Back the secret up together
  with the database: losing it detaches every player from their scores.
- Weak keys are refused: RSA below 2048 bits, DSA, and certificates (which
  flapd does not use).

**What SSH authentication does and does not give you:** it proves that the
client holds a particular private key, consistently over time. It does not
prove who a person is. Anyone can generate new keys, and in `open` mode
anyone can be a guest. Use `allowlist` mode for a private server.

### Host key

- An Ed25519 host key is created once (by `flapd init` or on first start)
  with mode 0600 and never replaced. flapd refuses to use host key or secret
  files that other users can read, as OpenSSH does.
- `flapd fingerprint` prints the SHA256 fingerprint for players to verify on
  first connect. Consider publishing it on your website or as an SSHFP DNS
  record (`ssh-keygen -r your.host -f key.pub`, combined with DNSSEC and
  `VerifyHostKeyDNS`).
- Losing `/var/lib/flapd` means a new host key, and every returning player
  sees a "host key changed" warning. `uninstall.sh` keeps that directory
  unless `--purge` is given.

### Abuse and resource exhaustion

Every limit is configurable, validated at startup and cannot be switched
off. Zero is rejected.

| Threat | Control |
|---|---|
| Connection floods | Global connection cap (64), global token bucket (20/s, burst 40), per-IP concurrency (3) and token bucket (12/min, burst 6). IPv6 is grouped by /64. All checks run before any cryptography. |
| Slow or silent handshakes (slowloris) | Max 16 concurrent handshakes; one 10 s deadline for key exchange and authentication |
| Password or key guessing | No passwords. Failed attempts (10 in 10 min) ban the address for 15 min. `MaxAuthTries` is 6 per connection. |
| Address-table exhaustion | The per-address table holds at most 10,000 entries, idle entries are swept, and it fails closed when full |
| Hogging games | 20 concurrent games, 2 per key, 1 session channel per connection |
| Idle squatting | 10 min input idle timeout, 2 h maximum session length, 10 s to send the shell request |
| Input floods | Input read in 1 KiB chunks and paced to 2 KiB/s (burst 8 KiB) through SSH flow control. Window changes limited to 10/s and coalesced. |
| Huge PTY sizes | Clamped to 400×200 before any allocation |
| Clients that stop reading | A write blocked for 20 s drops the connection. Output is bounded by the SSH window and one frame. |
| Dead peers | Keepalives every 30 s; dropped after 3 missed |
| Memory per channel | Bounded by x/crypto's 2 MiB channel window plus one frame. With 20 sessions the measured worst case is about 150 MB RSS, and systemd caps the service at 384 MB (`GOMEMLIMIT` 300 MiB). |
| CPU | Fixed-rate frames, no busy loops, at most 12 simulation steps per frame. 20 large sessions measured at about 0.6 cores; the systemd `CPUQuota` is one core. |
| Database stalls | Every query has a timeout (3 s). The UI never blocks on the database. |

### Terminal safety (output)

Everything shown to a player passes through one encoder
(`internal/canvas/encode.go`). It writes nothing but SGR colour sequences,
newlines and printable runes. Control characters (C0, DEL, C1), bidi
overrides and isolates, zero-width and format characters, private-use runes
and surrogates become `?`, and zero-width runes are dropped. Wide runes are
tracked so a row can never drift out of alignment. This holds even if the
database contained hostile names; the tests insert such names directly.

Display names are additionally restricted at the source:

- 3–16 characters of `[A-Za-z0-9._-]`, starting with a letter or digit;
- no names that look like `guest…` or like reserved words (`admin`, `root`,
  `server`, `flap`, …);
- unique under a look-alike key (case-insensitive, `0`→`o`, `1`/`i`→`l`,
  separators ignored).

The in-game name editor accepts only those characters.

When a session ends normally, Bubble Tea restores the alternate screen,
cursor, bracketed paste and keyboard modes before flapd closes the channel.
That is verified in tests and with OpenSSH in tmux. It is impossible if the
network drops, since nothing can be sent. Players can run `reset`.

### Score integrity

- Clients send only key presses. The world is simulated on the server, and
  the score is the number of pipes the simulated bird passed.
- Before saving, a plausibility check rejects any score above what the run's
  duration allows at maximum speed. That catches bugs, not players.
- Each run has a server-generated unique ID, so saving is idempotent. A run
  is written in one transaction together with the personal-best update, so
  concurrent sessions cannot corrupt bests or counts. The tests run 32
  concurrent writers.
- **Not prevented:** bots. A program can read the screen and press keys at
  the right moments; this is inherent to any game streamed to a client. Nor
  can it stop sybils (many keys or guests filling the leaderboard). The
  mitigations are the rate and session limits, `auth.guest_leaderboard =
  false`, `allowlist` mode, and moderation by deleting rows (see
  `OPERATIONS.md`).

### Privacy

- **The database holds:** the keyed identity hash (never the key or its
  fingerprint), display name, timestamps, best score, totals, display
  preferences, and run records (score, duration, ticks, flaps, course seed,
  end reason). It holds no IP addresses, SSH user names or client versions.
- **Logs** (stderr to journald) record connection lifecycle, authentication
  method, the SSH user name and client version (sanitised and truncated),
  terminal type and size, rejections, game start and end, and periodic
  resource statistics.
  - Players appear as a 12-character prefix of their identity hash.
  - Client IPs are logged according to `logging.ip_addresses`. The default
    `truncated` mode keeps /24 for IPv4 and /48 for IPv6. `hashed` uses an
    HMAC with a per-process random key (stable within a run, unlinkable
    across restarts), and `none` omits them.
  - Keystrokes and terminal data are never logged.
  - Logs are JSON, so a name or user string cannot forge a log line;
    untrusted strings are also reduced to printable ASCII.
- **Retention:** run history 365 days and 500 runs per player; idle guests
  below the leaderboard are removed after 30 days. journald retention is
  governed by your journald configuration.

### Process isolation (systemd)

`deploy/flapd.service` runs as the dedicated `flapd` system user, which has
no shell (`nologin`) and no sudo. It sets:

- `NoNewPrivileges`, empty `CapabilityBoundingSet` and `AmbientCapabilities`
  (port 2222 needs none);
- `ProtectSystem=strict` (only `/var/lib/flapd` is writable), `ProtectHome`,
  `PrivateTmp`, `PrivateDevices`, `PrivateUsers`;
- `ProtectKernel*`, `ProtectControlGroups`, `ProtectClock`,
  `ProtectHostname`, `ProtectProc=invisible`, `ProcSubset=pid`;
- `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`,
  `LockPersonality`, `MemoryDenyWriteExecute`, `RemoveIPC`;
- `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, a native-only
  `@system-service` syscall filter minus `@privileged`;
- `UMask=0077`, `LimitCORE=0` (no core dumps containing keys);
- `MemoryMax`, `CPUQuota`, `TasksMax`, `LimitNOFILE`.

`systemd-analyze security flapd.service` rates the unit **1.2 (OK)**. The
container test verifies at runtime that the process runs as `flapd`, has
`NoNewPrivs: 1`, has empty effective capabilities, and that the files are
0600.

### Supply chain and build

- Dependencies are pinned in `go.mod`/`go.sum`, and the toolchain is pinned
  with `toolchain go1.27.3`. The binary is static (`CGO_ENABLED=0`) and built
  with `-trimpath`.
- `make vuln` runs govulncheck. At the time of writing nothing reachable was
  found; the only module-level note concerns `x/crypto/openpgp`, which flapd
  does not import.
- The deploy scripts never download anything.

## What flapd does not protect against

- A compromised host or a malicious operator.
- Volumetric DDoS that saturates the network link. Use your provider's DDoS
  protection or rate limiting upstream.
- Sybil accounts and bots on the leaderboard (see above).
- Traffic analysis. SSH encrypts the content, but frame timing reveals
  activity.
- Terminal emulator bugs triggered by standard SGR and cursor sequences.
- Loss of the identity secret or host key without a backup.

## Reporting problems

Please report security issues privately to the operator of the server you
play on, or to the maintainers of this repository, before publishing details.
