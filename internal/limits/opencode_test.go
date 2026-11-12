package limits

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var openCodeNow = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

const openCodeUsage = `{"usage":{"rolling":{"percent":42.5,"resetsAt":"2026-11-01T03:00:00Z"},"weekly":{"percent":17,"resetsAt":"2026-11-03T00:00:00Z"},"monthly":{"percent":120,"resetsAt":"2026-11-18T01:24:12Z"}}}`

func TestOpenCodeUsageWindows(t *testing.T) {
	result, err := decodeOpenCodeUsage([]byte(openCodeUsage), openCodeNow)
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "opencode" || result.Plan != "Go" || result.Primary == nil || result.Secondary == nil {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Primary.UsedPercent != 42.5 || result.Primary.WindowMinutes != 300 || !result.Primary.ResetsAt.Equal(time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("rolling: %+v", result.Primary)
	}
	if result.Secondary.UsedPercent != 17 || result.Secondary.WindowMinutes != 10080 {
		t.Fatalf("weekly: %+v", result.Secondary)
	}
	if len(result.Extra) != 1 || result.Extra[0].Title != "Monthly" || result.Extra[0].UsedPercent != 100 || result.Extra[0].WindowMinutes != 0 || result.Extra[0].ResetsAt == nil {
		t.Fatalf("monthly: %+v", result.Extra)
	}
}

func TestOpenCodeSparseUsage(t *testing.T) {
	result, err := decodeOpenCodeUsage([]byte(`{"usage":{"rolling":{"usagePercent":0,"resetInSec":600},"monthly":{"percent":100}}}`), openCodeNow)
	if err != nil || result.Secondary != nil || result.Primary.ResetsAt == nil || !result.Primary.ResetsAt.Equal(openCodeNow.Add(10*time.Minute)) {
		t.Fatalf("unexpected sparse usage: %+v, %v", result, err)
	}
	if len(result.Extra) != 1 || result.Extra[0].ResetsAt != nil {
		t.Fatalf("monthly: %+v", result.Extra)
	}
	if _, err := decodeOpenCodeUsage([]byte(`{"usage":{"weekly":{"percent":1}}}`), openCodeNow); err == nil {
		t.Fatal("usage without a rolling window must fail")
	}
}

type openCodeFixture struct {
	dir      string
	database string
	statuses map[string]int
	keysSeen []string
	server   *httptest.Server
}

func newOpenCodeFixture(t *testing.T) *openCodeFixture {
	t.Helper()
	fixture := &openCodeFixture{dir: t.TempDir(), statuses: map[string]int{}}
	fixture.database = filepath.Join(fixture.dir, "opencode.db")
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != openCodeUsagePath {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		fixture.keysSeen = append(fixture.keysSeen, key)
		switch status := fixture.statuses[key]; status {
		case 0, http.StatusOK:
			io.WriteString(w, openCodeUsage)
		case http.StatusUnauthorized:
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"type":"authentication_error","code":"api_key_expired","message":"This API key has expired."}}`)
		default:
			w.WriteHeader(status)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *openCodeFixture) collector(envKey string) *Collector {
	collector := New("codexbar", slog.New(slog.NewTextHandler(io.Discard, nil)))
	collector.opencode = openCodeSource{dataDir: f.dir, database: f.database, envKey: envKey, baseURL: f.server.URL, client: f.server.Client(), now: func() time.Time { return openCodeNow }}
	return collector
}

func (f *openCodeFixture) credentials(t *testing.T, rows ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", f.database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{`CREATE TABLE IF NOT EXISTS credential (id TEXT PRIMARY KEY, integration_id TEXT, label TEXT, value TEXT, connector_id TEXT, method_id TEXT, active INTEGER, time_created INTEGER, time_updated INTEGER)`}
	if _, err := db.Exec(statements[0]); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		// row: integration|active(NULL,0,1)|value
		parts := strings.SplitN(row, "|", 3)
		var active any
		if parts[1] != "NULL" {
			active = parts[1]
		}
		if _, err := db.Exec(`INSERT INTO credential (id, integration_id, value, active, time_created) VALUES (?, ?, ?, ?, ?)`, string(rune('a'+i)), parts[0], parts[2], active, i); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *openCodeFixture) authFile(t *testing.T, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "auth.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeKeyPrecedence(t *testing.T) {
	fixture := newOpenCodeFixture(t)
	fixture.credentials(t,
		`opencode|NULL|{"type":"key","key":"old-console"}`,
		`opencode|1|{"type":"key","key":"console","metadata":{"server":"https://opencode.ai/console/"}}`,
		`opencode-go|1|{"type":"key","key":"self-hosted","metadata":{"server":"https://opencode.example.com/console"}}`,
		`opencode-go|0|{"type":"key","key":"signed-out"}`,
	)
	fixture.authFile(t, `{"opencode-go":{"type":"api","key":"auth-file"},"openai":{"type":"oauth"}}`)
	keys := fixture.collector("environment").opencode.keys(context.Background())
	if strings.Join(keys, ",") != "console,auth-file" {
		t.Fatalf("unexpected key order: %v", keys)
	}

	empty := newOpenCodeFixture(t)
	if keys := empty.collector("environment").opencode.keys(context.Background()); strings.Join(keys, ",") != "environment" {
		t.Fatalf("environment key should apply without auth.json: %v", keys)
	}
}

func TestOpenCodeFallsBackPastRejectedKeys(t *testing.T) {
	fixture := newOpenCodeFixture(t)
	fixture.credentials(t, `opencode|1|{"type":"key","key":"expired"}`, `opencode-go|1|{"type":"key","key":"zen-only"}`)
	fixture.authFile(t, `{"opencode-go":{"type":"api","key":"good"}}`)
	fixture.statuses["expired"] = http.StatusUnauthorized
	fixture.statuses["zen-only"] = http.StatusForbidden
	collector := fixture.collector("")
	collector.collect(context.Background(), "opencode", "api")
	providers := collector.Snapshot("UTC").Providers
	if len(providers) != 1 || providers[0].Stale || providers[0].Primary == nil || strings.Join(fixture.keysSeen, ",") != "expired,zen-only,good" {
		t.Fatalf("unexpected fallback: %+v, keys=%v", providers, fixture.keysSeen)
	}
}

func TestOpenCodeExpiredKeyIsReportedAndMissingSubscriptionIsOmitted(t *testing.T) {
	fixture := newOpenCodeFixture(t)
	fixture.authFile(t, `{"opencode-go":{"type":"api","key":"expired"}}`)
	fixture.statuses["expired"] = http.StatusUnauthorized
	collector := fixture.collector("")
	collector.collect(context.Background(), "opencode", "api")
	providers := collector.Snapshot("UTC").Providers
	if len(providers) != 1 || !providers[0].Stale || !strings.Contains(providers[0].Error, "This API key has expired.") {
		t.Fatalf("expired key should be reported: %+v", providers)
	}

	fixture.statuses["expired"] = http.StatusForbidden
	collector.collect(context.Background(), "opencode", "api")
	if providers := collector.Snapshot("UTC").Providers; len(providers) != 0 {
		t.Fatalf("a key without Go should be omitted: %+v", providers)
	}
	if _, err := newOpenCodeFixture(t).collector("").fetchOpenCode(context.Background()); !errors.Is(err, errNotConfigured) {
		t.Fatalf("no key should be omitted: %v", err)
	}
}
