<p align="center">
  <img src="assets/ccusage-hub.png" alt="ccusage-hub logo" width="160">
</p>

<h1 align="center">ccusage-hub</h1>

`ccusage-hub` combines token and cost usage from multiple computers without double-counting repeated reports. Each client runs [ccusage](https://github.com/ryoppippi/ccusage), sends cumulative snapshots to one small server, and the server exposes fleet-wide daily, source, model, device, and cost totals.

The project is designed for a trusted LAN or Tailnet. Version 1 intentionally has **no authentication or TLS**.

## How it works

On its first successful connection, every client submits up to 366 days of available history. It records that completion locally, then submits the last 30 days of cumulative usage every 10 minutes. The server keeps one high-water bucket for each `device + date + ccusage source`. If a Mac reports 100 tokens and later reports 130, the fleet total becomes 130—not 230. The complete model breakdown and client-calculated cost are replaced atomically with that bucket.

- Higher totals advance the high-water mark.
- Duplicate snapshots are idempotent.
- Lower totals remain in the audit history but do not reduce the fleet total.
- Device names are editable; generated UUIDs remain stable.
- Costs are stored as integer nano-dollars and returned as decimal USD strings.

## Install on the Raspberry Pi

Requirements: Go 1.24+ to build, and `ccusage` on `PATH` for the local client.

```bash
git clone https://github.com/trahane/ccusage-hub.git
cd ccusage-hub
./scripts/install-systemd.sh

curl http://127.0.0.1:7432/healthz

# After installing ccusage, enable this Pi's collector
systemctl --user enable --now ccusage-hub-client.service
```

The server listens on `0.0.0.0:7432`, stores SQLite data at `~/.local/share/ccusage-hub/ccusage-hub.db`, and is reachable over both the LAN and Tailscale.

## Install a macOS client

Install `ccusage`, download or build the `ccusage-hub` binary, and run:

```bash
npm install --global ccusage@20.0.20
```

```bash
./scripts/install-launchd.sh
```

The LaunchAgent targets `http://pibot:7432`. Change that URL to the Pi's LAN or Tailscale address when needed. On first run the client creates its config in `~/Library/Application Support/ccusage-hub/client.json`, including a stable UUID.

Run a client manually:

```bash
ccusage-hub client push --server http://pibot:7432
ccusage-hub client run --server http://pibot:7432 --interval 10m
ccusage-hub client rename --server http://pibot:7432 "Studio Mac mini"
```

Renaming changes only the display label. If the server is offline, the local name is saved and synchronized with the next snapshot.

## Server

```bash
ccusage-hub server \
  --listen 0.0.0.0:7432 \
  --database ./data/ccusage-hub.db \
  --timezone America/Los_Angeles
```

Environment equivalents are available as `CCUSAGE_HUB_LISTEN`, `CCUSAGE_HUB_DATABASE`, `CCUSAGE_HUB_TIMEZONE`, and `CCUSAGE_HUB_LOG_LEVEL`.

To add subscription quota windows, install CodexBar CLI and set
`CCUSAGE_HUB_CODEXBAR_COMMAND` to its executable path (or pass
`--codexbar-command`). Codex is collected every minute and Claude every three
minutes. Antigravity is collected every three minutes using `--source auto`.
The last successful result remains available if a later probe fails.

### Antigravity quotas
Use a CodexBar CLI version that supports `antigravity` and verify access on the
hub host with `codexbar usage --provider antigravity --source auto --format json`.
The collector runs as the server user, so its signed-in Antigravity app or CodexBar
Antigravity credentials must be available to that user on that machine. A hub on
another machine does not automatically read a client’s Antigravity session.

Antigravity `primary` is the Gemini Models quota pool and `secondary` is the
Claude and GPT pool, not five-hour and weekly windows. Additional named model
quotas are preserved in `extra`; missing pools stay absent. Collection failures
mark the last successful reading stale. CodexBar does not provide Antigravity
token/cost history; the normal usage ingestion accepts it if a client reports it.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/v1/snapshots` | Submit a cumulative client snapshot |
| `GET` | `/api/v1/usage?days=7` | Daily fleet totals with source, model, and device breakdowns |
| `GET` | `/api/v1/limits` | Cached Codex, Claude, and Antigravity quota windows and reset times |
| `GET` | `/api/v1/models?days=30&source=codex` | Model-level usage and cost totals |
| `GET` | `/api/v1/devices` | Devices, names, last-seen timestamps, usage, and costs |
| `PATCH` | `/api/v1/devices/{uuid}` | Rename a client |
| `GET` | `/api/v1/info` | Version, timezone, and capabilities |
| `GET` | `/healthz` | Server and database health |

Example dashboard query:

```bash
curl 'http://pibot:7432/api/v1/usage?days=7'
```

Use ccusage 20.0.20 or newer: the older 20.0.17 can count repeated Codex token-status
events more than once. Upgrade ccusage on each collector host, not only the hub server.

Collectors explicitly enable online pricing (`--no-offline`) so ccusage can refresh model
rates instead of relying only on its bundled pricing snapshot. The hub preserves ccusage's
input, cached-input, output, and cost breakdowns; ccusage handles session and fork accounting.
A fork's new requests, including processing inherited context, contribute usage. Copied
historical usage records must not be added again as new requests.

After updating an existing collector, refresh available historical costs with:

```bash
ccusage-hub client push --lookback-days 366
```

Newer snapshots with unchanged token totals can replace costs. If local history now reports
fewer tokens, the high-water rule retains the existing bucket; this command does not force
those lower observations into the totals.

All cost fields use decimal USD strings, for example `"13.44311715"`. Costs come directly from each client's pricing data, so the server records the submitting `ccusage` version. If source totals differ from model rows, the source remains authoritative and the server records an ingestion warning.

See [docs/api.md](docs/api.md) for the complete request contract and response semantics.

## Configuration and recovery

Client configuration lives in the operating system's standard user config directory. Back it up to preserve the device UUID across reinstalls. Display names need not be unique.

`backfillDays` defaults to 366 and `lookbackDays` defaults to 30. After the first successful historical snapshot, the client writes `backfilledAt` to its config. Removing only `backfilledAt` safely repeats the backfill; the server's high-water buckets prevent double-counting.

To deliberately accept a lower observation after local logs were corrected or deleted:

```bash
ccusage-hub admin rebase \
  --database ~/.local/share/ccusage-hub/ccusage-hub.db \
  --device DEVICE_UUID \
  --date 2026-07-10 \
  --source codex
```

Daily usage totals and model breakdowns are retained permanently. Raw snapshots
and observation audit history are retained for seven days; consecutive identical
observations share one row. The latest observation for each device/date/source is
kept even after that window so `admin rebase` continues to work for old history.
Snapshot metadata referenced by totals or observations remains, but its expired
payload and warnings are removed. Snapshot-ID deduplication is guaranteed during
the audit window; older replayed uploads still follow the high-water rules.

The server prunes on startup and hourly, truncates the SQLite WAL every minute,
and compacts when at least 16 MiB and 25% of the database are free. Connection
settings also enable automatic checkpoints and an 8 MiB journal size target.
An external reader holding a transaction can delay truncation; blocked checkpoints
are logged and retried. Back up the database using SQLite's backup API, or stop
the server before copying it. Never delete the WAL manually.

To reclaim an existing oversized database immediately, stop the server, take a
backup, then run:

```bash
ccusage-hub admin compact --database ~/.local/share/ccusage-hub/ccusage-hub.db
```

Restart the server after compaction. This removes expired audit records while
preserving daily totals, device names, and the latest observations for rebase.

## Development

```bash
make test
make check
make build
```

Releases provide native Linux and macOS binaries for AMD64 and ARM64. See `config/` and `deploy/` for examples.

## License

MIT
