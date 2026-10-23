# Operations guide

These instructions are for running flapd on a Debian or Ubuntu VPS with
systemd. Every command was exercised by `deploy/test/systemd-test.sh`, which
runs in disposable Debian 12 and Ubuntu 24.04 containers with systemd as
PID 1.

**Contents:** [Requirements](#requirements) ·
[Deploy](#deploy) · [Files](#files) ·
[Configuration changes](#configuration-changes) ·
[Logs and health](#logs-and-health) · [Upgrade](#upgrade) ·
[Rollback](#rollback) · [Backup and restore](#backup-and-restore) ·
[Maintenance mode and restarts](#maintenance-mode-and-restarts) ·
[Private servers (allowlist)](#private-servers-allowlist) ·
[Moderation](#moderation) · [Capacity planning](#capacity-planning) ·
[Uninstall](#uninstall) · [Troubleshooting](#troubleshooting) ·
[Client compatibility checklist](#client-compatibility-checklist)

## Requirements

- Linux on x86_64 or arm64 with systemd. Debian 12 and Ubuntu 24.04 are
  tested. Other systemd distributions should work, and systemd versions older
  than 254 ignore the `RestartSteps` backoff keys.
- One free TCP port (default **2222**). Keep your administrative sshd on
  port 22.
- About 30 MB of disk plus the database, and 100–400 MB of RAM depending on
  the number of players.
- Go 1.26+ wherever you build. The VPS needs neither Go nor any other runtime
  dependency.

## Deploy

1. **Build** (on your workstation or the server):

   ```sh
   make build                 # bin/flapd, static, for the build machine's architecture
   make build-arm64           # bin/flapd-linux-arm64, for ARM VPSes
   ```

   Builds are reproducible: go.mod pins `toolchain go1.27.3`, and the build
   uses `CGO_ENABLED=0 -trimpath` with all dependencies pinned in `go.sum`.

2. **Copy** the checkout (the scripts need `deploy/`) and the binary:

   ```sh
   rsync -a --exclude .git ./ admin@your-vps:flap/
   # for arm64: cp bin/flapd-linux-arm64 bin/flapd on the server
   ```

3. **Install:**

   ```sh
   ssh admin@your-vps
   cd flap && sudo ./deploy/install.sh            # or: --port 2323, --no-start
   ```

   The installer:
   - checks prerequisites;
   - creates the `flapd` system user (no shell, no sudo);
   - creates `/etc/flapd` (0750) and `/var/lib/flapd` (0700);
   - installs `/etc/flapd/flapd.toml` only if it does not exist yet;
   - validates the configuration, refusing port 22 unless `--allow-port-22`;
   - checks that the port is free;
   - creates the host key and identity secret;
   - installs `/usr/local/bin/flapd` and the hardened unit;
   - starts the service and waits for it to pass `flapd healthcheck`.

   It finishes by printing the host key fingerprint and connect command. It
   never changes sshd, firewall rules or other services.

4. **Open the firewall** yourself:

   ```sh
   sudo ufw allow 2222/tcp comment 'flapd'                                         # ufw
   sudo firewall-cmd --permanent --add-port=2222/tcp && sudo firewall-cmd --reload # firewalld
   sudo nft add rule inet filter input tcp dport 2222 accept                       # nftables
   ```

   Also allow the port in your cloud provider's security group if it has one.
   Do not remove the rule that allows your administrative SSH.

5. **Publish the fingerprint** that `flapd fingerprint -config
   /etc/flapd/flapd.toml` prints, so players can verify it on first connect.

6. **Play:** `ssh -p 2222 flappy@your-vps`.

## Files

| Path | Owner / mode | Contents |
|---|---|---|
| `/usr/local/bin/flapd` | root 0755 | The binary (`flapd.prev` is the previous version after an upgrade) |
| `/etc/flapd/flapd.toml` | root:flapd 0640 | Configuration |
| `/etc/systemd/system/flapd.service` | root 0644 | The unit |
| `/var/lib/flapd/ssh_host_ed25519_key` | flapd 0600 | Host key; players pin it |
| `/var/lib/flapd/identity_secret` | flapd 0600 | Secret for player identities |
| `/var/lib/flapd/flap.db` (+`-wal`, `-shm`) | flapd 0600 | Players, runs, leaderboard |
| `/var/lib/flapd/backups/` | flapd 0700 | Automatic pre-upgrade backups (newest 10) |
| `/usr/local/share/doc/flapd/` | root | Copies of the documentation |

## Configuration changes

```sh
sudoedit /etc/flapd/flapd.toml
sudo -u flapd flapd check-config -config /etc/flapd/flapd.toml   # lists every problem at once
sudo systemctl restart flapd                                       # players get a notice; runs are saved
```

`systemctl reload flapd` (SIGHUP) only reloads the allowlist; every other
setting needs a restart. Always run `check-config` before restarting:
`systemctl restart` stops the running server first, and an invalid file then
makes `ExecStartPre` fail. The service stays down until the file is fixed,
with the errors shown in `systemctl status flapd`.

To change resource limits enforced by systemd (`MemoryMax`, `CPUQuota`,
`TasksMax`, `LimitNOFILE`), use a drop-in so upgrades keep it:

```sh
sudo systemctl edit flapd
#   [Service]
#   MemoryMax=768M
#   Environment=GOMEMLIMIT=600MiB
sudo systemctl restart flapd
```

Keep `GOMEMLIMIT` at about 80% of `MemoryMax`.

## Logs and health

```sh
systemctl status flapd
journalctl -u flapd -f                          # JSON lines; add -o cat for compactness
journalctl -u flapd --since today | grep -E 'rejected|banned|panic|failed'
sudo -u flapd flapd healthcheck -config /etc/flapd/flapd.toml      # completes a handshake, checks the host key
sudo -u flapd flapd check-db -config /etc/flapd/flapd.toml         # integrity + counts
```

Useful log messages:

- `connection authenticated` / `connection closed`
- `game started` / `game ended`, with reason and best score
- `connection rejected` (debug level), with reason `ip_rate`, `banned`,
  `ip_concurrency`, `global_rate` or `tracking_full`
- `address banned after repeated authentication failures`
- `client stopped reading output`, `peer stopped answering keepalives`
- `stats`, every 5 minutes: sessions, connections, goroutines, heap,
  `open_fds`, totals and rejection counters

The healthcheck stops right after verifying the host key, so it never counts
as a failed login. It is suitable for cron, a systemd timer or external
monitoring through an SSH port check.

journald handles log rotation; set `SystemMaxUse=` in
`/etc/systemd/journald.conf` to bound disk use.

## Upgrade

```sh
git pull && make build           # or copy in a new bin/flapd
sudo ./deploy/install.sh
```

On an existing installation the installer:

1. validates the existing configuration with the new binary before stopping
   anything;
2. writes a verified database backup to
   `/var/lib/flapd/backups/flap-<UTC time>.db` (keeping the newest 10);
3. keeps the old binary as `/usr/local/bin/flapd.prev`;
4. installs the new binary and unit, and restarts. Players get a "server is
   restarting" notice and in-progress runs are saved;
5. waits up to 30 s for a passing health check. If the check fails, it
   restores `flapd.prev`, restarts and reports the failure.

## Rollback

```sh
sudo ./deploy/rollback.sh        # swaps flapd.prev back in, restarts and health-checks
```

If the newer version migrated the database schema, the older binary refuses
to start with "database schema is newer than this binary". Restore the backup
taken before the upgrade (see below), then run the rollback again.

## Backup and restore

The database can be backed up while the service runs:

```sh
sudo -u flapd flapd backup -config /etc/flapd/flapd.toml -out /var/lib/flapd/backups/manual-$(date +%F).db
```

`backup` uses SQLite's `VACUUM INTO`, a consistent snapshot, and verifies the
copy with a full integrity check. For a complete disaster-recovery copy, also
save the two secrets once (they never change):

```sh
sudo tar -C /var/lib/flapd -czf flapd-secrets.tgz ssh_host_ed25519_key identity_secret
```

Store the secrets encrypted and off the server. Without them, players see a
new host key and lose their link to existing scores.

To **restore**:

```sh
sudo systemctl stop flapd
sudo install -o flapd -g flapd -m 0600 BACKUP.db /var/lib/flapd/flap.db
sudo rm -f /var/lib/flapd/flap.db-wal /var/lib/flapd/flap.db-shm
# to restore the secrets as well:
sudo tar -C /var/lib/flapd -xzf flapd-secrets.tgz && sudo chown flapd:flapd /var/lib/flapd/* && sudo chmod 600 /var/lib/flapd/*_key /var/lib/flapd/identity_secret
sudo -u flapd flapd check-db -config /etc/flapd/flapd.toml -full
sudo systemctl start flapd
```

If the startup integrity check ever fails, the service refuses to start and
says so. Restore the latest backup as above. Use
`storage.integrity_check = "full"` for a slower, deeper check at startup.

## Maintenance mode and restarts

Set `server.maintenance = true` (and optionally `maintenance_message`), then
restart. New connections see the message and disconnect cleanly. A plain
`systemctl restart` or `stop` is already graceful:

1. new connections are refused;
2. players see a notice;
3. in-progress runs are saved;
4. flapd waits up to `server.shutdown_grace` (10 s).

## Private servers (allowlist)

```toml
[auth]
mode = "allowlist"
authorized_keys_file = "/etc/flapd/authorized_keys"
```

```sh
sudo install -o root -g flapd -m 0640 players.pub /etc/flapd/authorized_keys   # OpenSSH format, one key per line
sudo systemctl reload flapd                                                     # reload after edits
```

Key options such as `command=` are rejected rather than ignored.

## Moderation

```sh
sudo -u flapd flapd remove-player -config /etc/flapd/flapd.toml -name SomeName
```

This deletes the player and their runs, and the leaderboard updates
immediately. It is safe while the service runs. To stop guests from being
ranked at all, set `auth.guest_leaderboard = false` and restart.

## Capacity planning

Measured with `flapload` on a Ryzen 5 5600X (see the README). Expect a
small VPS to be somewhat slower.

| Players (terminal) | CPU | RSS | Bandwidth out |
|---|---|---|---|
| 20 (100×30) | ~0.4 core | ~80 MB | ~1.1 MB/s total |
| 20 (160×50) | ~0.6 core | ~150 MB | ~1.7 MB/s total |

Per player that is about 55–85 KB/s on average, roughly 0.2–0.3 GB per hour
of play. Lower `ui.render_fps` to save bandwidth (bytes scale with FPS).
Raise `limits.max_sessions` only together with the systemd memory and CPU
limits. Run load tests only against servers you own:

```sh
./bin/flapload -addr 127.0.0.1:2222 -n 20 -duration 60s -pid "$(pidof flapd)" -hostkey host.pub
```

Load testing from one machine also needs the per-IP limits raised
(`max_connections_per_ip`, `connection_rate_per_ip`), because every client
shares one address.

## Uninstall

```sh
sudo ./deploy/uninstall.sh             # removes service and binary; keeps /etc/flapd and /var/lib/flapd
sudo ./deploy/uninstall.sh --purge     # also deletes data, config and the flapd user (asks to type 'delete')
```

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| `Connection refused` | Service down (`systemctl status flapd`), wrong port, or firewall/security group closed |
| Connection closes immediately, before any prompt | Admission limits: too many connections from your address, rate limit or ban. `journalctl -u flapd` shows `connection rejected` with a reason (debug level) and the `stats` counters. Wait, or raise the limits. |
| `Permission denied (publickey,keyboard-interactive)` | `publickey` or `allowlist` mode and your key isn't accepted. Check `auth.mode` and the allowlist; RSA keys must be ≥2048 bits. |
| `Permission denied (publickey)` | Guests are disabled (`auth.mode = "publickey"`) and no key was offered |
| `WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED` | The host key was lost, for example by reinstalling with `--purge` or a new VPS. Restore `ssh_host_ed25519_key` from backup, or tell players the new fingerprint. |
| "Flap is full right now" | `limits.max_sessions` reached |
| "You already have N games open" | `limits.max_sessions_per_player`. Close other tabs, or check that your SSH agent isn't sharing one key across many sessions. |
| "needs an interactive terminal" | The client didn't request a PTY (`ssh -T`, a command, or piped stdin). Use `ssh -t`. |
| `exec request failed` | A command was passed (`ssh host something`). Connect without a command. |
| Keys need Enter, or nothing happens | No PTY was allocated; use `ssh -t`. Some multiplexers swallow Esc; use `q`. |
| Colours look wrong or garish | Press `C` to cycle truecolor → 256 → 16. macOS Terminal before macOS 26 lacks truecolor. |
| Broken boxes, `?` or misaligned blocks | Fonts without block characters, or CJK fonts with ambiguous-width glyphs. Press `U` for ASCII mode. |
| Game paused with "RESIZED" | It pauses whenever the window size changes; press Space |
| Terminal is weird after the connection dropped | The server couldn't restore it. Run `reset` (or `tput rmcup; stty sane`), or open a new tab. |
| A player shows up as a new name | Their client offered a different key. Use `-i KEY -o IdentitiesOnly=yes`. |
| `cannot listen on ...: the port is already in use` | Another process owns the port (`ss -ltnp 'sport = :2222'`). Change `server.listen` or stop the other service. |
| `database schema is newer than this binary` | You rolled back past a migration. Restore the pre-upgrade backup. |
| `database integrity check failed` | Restore from backup (see above) |
| `host key ... is accessible by other users` | Run `sudo chmod 600 /var/lib/flapd/ssh_host_ed25519_key /var/lib/flapd/identity_secret` |
| High bandwidth | Lower `ui.render_fps`. Truecolor clients use about 70% more than 256-colour clients. |
| No sound | Sound is the terminal bell. Check that it is on (`S`; the welcome screen shows "S sound: on") and that the terminal's bell is audible: Windows Terminal `bellStyle` (audible by default), GNOME/Konsole/kitty bell settings, or tmux `bell-action`. Alacritty and foot are silent by default. |
| Too many beeps | Press `S` to turn sound off (remembered per key), or set `ui.default_sound = "off"` for everyone |

## Client compatibility checklist

These were tested automatically on Linux: OpenSSH 10.5 in tmux (TERM
`tmux-256color`), and the Go x/crypto client through a VT emulator. For the
following clients, which were not tested here, run this checklist once
against your server:

| Check | Windows Terminal + Win32-OpenSSH | PowerShell / conhost | macOS Terminal | iTerm2 | PuTTY |
|---|---|---|---|---|---|
| Welcome card frame aligned; title and bird visible | | | | | |
| Space starts, flaps; P pauses; L, H, N work | | | | | |
| Resizing the window shows "RESIZED"; the game re-lays out | | | | | |
| Shrinking below 50×18 shows the notice; growing restores | | | | | |
| C cycles colours; U switches to ASCII | | | | | |
| Q, Esc and Ctrl+C exit; the prompt returns on a normal screen with a visible cursor | | | | | |
| Leaderboard shows your score after reconnecting with the same key | | | | | |

Known considerations:

- Windows 10/11 include `ssh.exe` (Win32-OpenSSH). It requests a PTY and
  sends `TERM=xterm-256color`, so the game defaults to 256 colours; Windows
  Terminal also handles truecolor via <kbd>C</kbd>. flapd always sends CR LF,
  so line handling does not depend on the console mode.
- On the legacy console host with raster fonts, block characters may be
  missing; use <kbd>U</kbd>.
- PuTTY: set Window → Translation to UTF-8, and Connection → Data →
  Terminal-type string to `xterm-256color`.
