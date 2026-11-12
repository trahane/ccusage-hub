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

Limits are stored independently from token/cost snapshots. This endpoint is always
available, including when no server-side CodexBar collector is configured. Empty
collection returns `providers: []`. Both `limits` and `limits-reporting` appear in
`/api/v1/info` capabilities.

The schema remains version 1. The existing `providers` array contains one selected
account per provider, preserving the Android/iOS wire format. Each window contains
`usedPercent`, `windowMinutes`, and nullable RFC 3339 `resetsAt`. `primary`,
`secondary`, and named `extra` windows retain their existing meaning. Antigravity
`primary` is Gemini Models and `secondary` is Claude/GPT; `extra` preserves named
five-hour and weekly quotas. Unknown quota values are omitted rather than shown as
exhausted.

Additional fields identify the `accountId`, `accountLabel`, `deviceId`, `deviceName`,
`checkedAt`, and `receivedAt`. `updatedAt` is the original successful observation
time, not the time the phone fetched the cache. A reading becomes stale after ten
minutes without a current observation, or when its reporting device fails or
switches accounts. Last good windows remain available with `stale: true` and an
`error`. A failed device cannot invalidate a healthy device's reading of the same
account. Whole observations are selected; percentages are never added, nor are
windows from different accounts or observations combined.

The additive `accounts` array lists the selected reading for every known
provider/account pair. The first successful account for each provider becomes its
persistent display default; another account does not silently replace it. Use
`GET /api/v1/limits?accountId=ID` to read another account, or change the default on
the server with:

```bash
ccusage-hub admin select-limit-account --database PATH --provider antigravity --account ID
```

### Report limits

`POST /api/v1/limits`

The maximum body is 256 KiB. It accepts no token counts, cost history, or credentials.
Unknown fields are rejected. Device UUIDs are shared with cost reporting, but a
device can report limits before submitting any cost snapshots.

```json
{
  "schemaVersion": 1,
  "device": {
    "id": "11111111-1111-4111-8111-111111111111",
    "name": "MacBook Pro",
    "platform": "darwin-arm64",
    "clientVersion": "1.0.0"
  },
  "checkedAt": "2026-11-12T10:45:00Z",
  "providers": [{
    "provider": "antigravity",
    "accountId": "account:stable-provider-account-hash",
    "source": "cli",
    "primary": { "usedPercent": 2, "windowMinutes": 10080, "resetsAt": null },
    "secondary": { "usedPercent": 0, "windowMinutes": 300, "resetsAt": null },
    "extra": [],
    "updatedAt": "2026-11-12T10:44:55Z"
  }]
}
```

A report contains one active-account check per provider; the built-in client sends
each provider independently. Report a failed check with `stale: true`, a concise
`error`, and the check time in `updatedAt`. If identity is unavailable on a failed
check, the hub applies the failure to that device's last account for the provider.
It retains the previous successful observation and timestamp. Replayed or
out-of-order checks do not overwrite newer ones. Submissions return `202 Accepted`,
including identical retries. Readings survive server restarts.

Account IDs are provider-scoped hashes of the identity and organization, never
credential/token hashes. If the provider does not expose an identity, it is scoped
to the reporting device so unrelated accounts are not merged. An explicit
`limitAccounts` alias in client configuration can link unidentified accounts;
configure the same alias only for devices you know use the same account. The
current client collects the active account for each provider; it does not enumerate
or authenticate additional saved accounts.

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
