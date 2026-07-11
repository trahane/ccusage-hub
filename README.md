# ccusage-hub

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
npm install --global ccusage@20.0.17
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

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/v1/snapshots` | Submit a cumulative client snapshot |
| `GET` | `/api/v1/usage?days=7` | Daily fleet totals with source, model, and device breakdowns |
| `GET` | `/api/v1/models?days=30&source=codex` | Model-level usage and cost totals |
| `GET` | `/api/v1/devices` | Devices, names, last-seen timestamps, usage, and costs |
| `PATCH` | `/api/v1/devices/{uuid}` | Rename a client |
| `GET` | `/api/v1/info` | Version, timezone, and capabilities |
| `GET` | `/healthz` | Server and database health |

Example dashboard query:

```bash
curl 'http://pibot:7432/api/v1/usage?days=7'
```

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

Raw compressed snapshots and normalized observations are retained indefinitely. Back up the SQLite database according to your storage requirements.

## Development

```bash
make test
make check
make build
```

Releases provide native Linux and macOS binaries for AMD64 and ARM64. See `config/` and `deploy/` for examples.

## License

MIT
