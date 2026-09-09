# HTTP API v1

The API is unauthenticated and intended for trusted LAN or Tailnet use. JSON responses use `Cache-Control: no-store`; CORS permits all origins.

## Submit a snapshot

`POST /api/v1/snapshots`

The maximum body size is 5 MiB. `snapshotId` and `device.id` are UUIDs. Dates use `YYYY-MM-DD`, timestamps use RFC 3339, counters are nonnegative integers, and USD costs are nonnegative decimal strings with up to nine stored fractional digits.

```json
{
  "schemaVersion": 1,
  "snapshotId": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  "capturedAt": "2026-07-10T19:35:00Z",
  "device": {
    "id": "11111111-1111-4111-8111-111111111111",
    "name": "Studio Mac mini",
    "platform": "darwin-arm64",
    "clientVersion": "1.0.0"
  },
  "ccusage": { "version": "20.0.17", "lookbackDays": 30 },
  "days": [{
    "date": "2026-07-10",
    "sources": [{
      "source": "codex",
      "inputTokens": 100,
      "outputTokens": 20,
      "cacheReadTokens": 500,
      "cacheCreationTokens": 0,
      "totalTokens": 620,
      "costUSD": "1.25",
      "models": [{
        "modelName": "gpt-example",
        "inputTokens": 100,
        "outputTokens": 20,
        "cacheReadTokens": 500,
        "cacheCreationTokens": 0,
        "totalTokens": 620,
        "costUSD": "1.25"
      }]
    }]
  }]
}
```

New snapshots return `202 Accepted`; duplicate IDs return `200 OK`. The response includes `status`, applied/ignored bucket counts, warnings, and the token and cost delta.

## Read usage

`GET /api/v1/usage?days=7`

`days` accepts 1–366 and defaults to 7. The response contains fleet totals and daily rows. Each day contains source totals; each source contains model and device breakdowns. Every aggregate exposes input, output, cache-read, cache-creation, total tokens, and `costUSD`.

`GET /api/v1/models?days=30&source=codex`

Returns model totals across the requested range. `source` is optional.

`GET /api/v1/devices`

Returns device identity, display name, platform, client version, first/last seen timestamps, and authoritative usage totals.

## Read subscription limits

`GET /api/v1/limits`

When the optional CodexBar collector is configured, this endpoint returns cached
Codex and Claude five-hour, weekly, and additional quota windows, plus Antigravity
model quotas. For `antigravity`, `primary` represents Gemini Models and `secondary`
represents Claude and GPT; additional named quotas appear in `extra`. Missing
quota pools are omitted, and durations may be zero when unknown. Each window
contains `usedPercent`, `windowMinutes`, and an RFC 3339 `resetsAt` timestamp.
Provider entries include their source, CLI version, last successful update, and a
stale error while retaining the last good windows if a later collection fails.

The matching `limits` capability appears in `/api/v1/info` only when the collector
is configured.

## Rename a device

`PATCH /api/v1/devices/{deviceId}`

```json
{ "name": "Studio Mac mini" }
```

Names are trimmed UTF-8 strings containing 1–64 characters and need not be unique. UUID identity and usage buckets do not change.

## Metadata and health

- `GET /api/v1/info` returns server version, schema version, canonical timezone, and capabilities.
- `GET /healthz` verifies that the process and SQLite database are available.

Errors use this shape:

```json
{ "error": { "status": 400, "message": "description" } }
```
