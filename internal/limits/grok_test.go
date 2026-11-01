package limits

import (
	"context"
	"encoding/json"
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

var grokNow = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// Recorded from cli-chat-proxy.grok.com for an X Premium account.
const grokWeeklyBilling = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-28T23:27:47.302877+00:00","end":"2026-11-04T23:27:47.302877+00:00"},"creditUsagePercent":100.0,"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"productUsage":[{"product":"GrokBuild","usagePercent":97.0},{"product":"GrokChat","usagePercent":3.0}],"isUnifiedBillingUser":true,"prepaidBalance":{"val":0},"billingPeriodStart":"2026-10-28T23:27:47.302877+00:00","billingPeriodEnd":"2026-11-04T23:27:47.302877+00:00"}}`

type grokTestServer struct {
	*httptest.Server
	billing string
	tokens  []string
}

func newGrokTestServer(t *testing.T, billing string) *grokTestServer {
	t.Helper()
	server := &grokTestServer{billing: billing}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-xai-token-auth") != "xai-grok-cli" {
			t.Errorf("missing Grok CLI auth header on %s", r.URL)
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		server.tokens = append(server.tokens, token)
		if token == "revoked" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.RequestURI() {
		case "/v1/billing?format=credits":
			io.WriteString(w, server.billing)
		case "/v1/settings":
			io.WriteString(w, `{"subscription_tier_display":"X Premium","goal_planner_enabled":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newGrokCollector(t *testing.T, server *grokTestServer, auth string) (*Collector, string) {
	t.Helper()
	home := t.TempDir()
	writeGrokAuth(t, home, auth)
	collector := New("codexbar", slog.New(slog.NewTextHandler(io.Discard, nil)))
	collector.grok = grokSource{home: home, command: "grok", baseURL: server.URL, client: server.Client(), now: func() time.Time { return grokNow }}
	collector.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("unexpected Grok CLI refresh")
		return nil, nil
	}
	return collector, home
}

func writeGrokAuth(t *testing.T, home, auth string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
}

func grokAuth(key, expiresAt string) string {
	return `{"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828":{"key":"` + key + `","auth_mode":"oidc","refresh_token":"r","expires_at":"` + expiresAt + `"}}`
}

func TestGrokWeeklyQuotaIsSecondary(t *testing.T) {
	server := newGrokTestServer(t, grokWeeklyBilling)
	collector, _ := newGrokCollector(t, server, grokAuth("token", "2026-11-01T05:00:00.369530Z"))
	collector.collect(context.Background(), "grok", "cli-proxy")
	grok := collector.Snapshot("UTC").Providers[0]
	if grok.Stale || grok.Provider != "grok" || grok.Source != "grok-cli-proxy" || grok.Plan != "X Premium" || grok.Primary != nil {
		t.Fatalf("unexpected grok limits: %+v", grok)
	}
	want := time.Date(2026, 11, 4, 23, 27, 47, 302877000, time.UTC)
	if grok.Secondary == nil || grok.Secondary.UsedPercent != 100 || grok.Secondary.WindowMinutes != 10080 || !grok.Secondary.ResetsAt.Equal(want) {
		t.Fatalf("weekly quota: %+v", grok.Secondary)
	}
}

func TestGrokOmittedPercentInActivePeriodIsZero(t *testing.T) {
	result, err := decodeGrokBilling(grokBillingJSON(t, `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-10-15T00:00:00Z","end":"2026-11-15T00:00:00Z"}}}`), grokNow)
	if err != nil || result.Secondary == nil || result.Secondary.UsedPercent != 0 || result.Secondary.WindowMinutes != 31*24*60 {
		t.Fatalf("unexpected implicit zero: %+v, %v", result.Secondary, err)
	}
	if _, err := decodeGrokBilling(grokBillingJSON(t, `{"config":{"currentPeriod":{"end":"2026-11-15T00:00:00Z"}}}`), grokNow); err == nil {
		t.Fatal("a period without a start must not become 0% used")
	}
}

func TestGrokShortWindowIsPrimaryAndPercentIsClamped(t *testing.T) {
	result, err := decodeGrokBilling(grokBillingJSON(t, `{"config":{"creditUsagePercent":140,"currentPeriod":{"start":"2026-10-31T22:00:00Z","end":"2026-11-01T03:00:00Z"}}}`), grokNow)
	if err != nil || result.Secondary != nil || result.Primary == nil || result.Primary.WindowMinutes != 300 || result.Primary.UsedPercent != 100 {
		t.Fatalf("unexpected short window: %+v, %v", result, err)
	}
}

func TestGrokCredentialSelection(t *testing.T) {
	home := t.TempDir()
	writeGrokAuth(t, home, `{"https://accounts.x.ai/sign-in":{"key":"legacy"},"https://auth.x.ai::other":{"key":"","auth_mode":"oidc"},"https://auth.x.ai::client":{"key":"oidc","auth_mode":"oidc"},"https://api.x.ai":{"key":"k","auth_mode":"api_key"},"version":2}`)
	if credential, err := readGrokCredential(home); err != nil || credential.Key != "oidc" {
		t.Fatalf("expected OIDC login: %+v, %v", credential, err)
	}
	writeGrokAuth(t, home, `{"https://accounts.x.ai/sign-in":{"key":"legacy"}}`)
	if credential, err := readGrokCredential(home); err != nil || credential.Key != "legacy" {
		t.Fatalf("expected legacy login: %+v, %v", credential, err)
	}
	writeGrokAuth(t, home, `{"https://api.x.ai":{"key":"k","auth_mode":"api_key"}}`)
	if _, err := readGrokCredential(home); err == nil {
		t.Fatal("an API key has no subscription quota")
	}
	if _, err := readGrokCredential(t.TempDir()); !errors.Is(err, errGrokNotSignedIn) {
		t.Fatalf("missing login should explain how to sign in: %v", err)
	}
}

func TestGrokIsOmittedUntilSignedIn(t *testing.T) {
	server := newGrokTestServer(t, grokWeeklyBilling)
	collector, home := newGrokCollector(t, server, grokAuth("token", "2026-11-01T05:00:00Z"))
	collector.collect(context.Background(), "grok", "cli-proxy")
	if len(collector.Snapshot("UTC").Providers) != 1 {
		t.Fatal("expected Grok limits")
	}
	if err := os.Remove(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	collector.collect(context.Background(), "grok", "cli-proxy")
	if providers := collector.Snapshot("UTC").Providers; len(providers) != 0 {
		t.Fatalf("signed-out Grok should be omitted: %+v", providers)
	}
}

func TestGrokRefreshesExpiringLoginThroughCLI(t *testing.T) {
	server := newGrokTestServer(t, grokWeeklyBilling)
	collector, home := newGrokCollector(t, server, grokAuth("old", "2026-11-01T00:01:00Z"))
	collector.run = func(_ context.Context, command string, args ...string) ([]byte, error) {
		if command != "grok" || len(args) != 1 || args[0] != "models" {
			t.Fatalf("unexpected refresh command: %s %v", command, args)
		}
		writeGrokAuth(t, home, grokAuth("new", "2026-11-01T06:00:00Z"))
		return nil, nil
	}
	if _, err := collector.fetchGrok(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, token := range server.tokens {
		if token != "new" {
			t.Fatalf("used expiring token: %v", server.tokens)
		}
	}
}

func TestGrokRetriesOnceAfterRejectedLogin(t *testing.T) {
	server := newGrokTestServer(t, grokWeeklyBilling)
	collector, home := newGrokCollector(t, server, grokAuth("revoked", "2026-11-01T05:00:00Z"))
	refreshes := 0
	collector.run = func(context.Context, string, ...string) ([]byte, error) {
		refreshes++
		writeGrokAuth(t, home, grokAuth("new", "2026-11-01T06:00:00Z"))
		return nil, nil
	}
	if result, err := collector.fetchGrok(context.Background()); err != nil || refreshes != 1 || result.Secondary == nil {
		t.Fatalf("expected one refresh and a quota: %+v, %v, refreshes=%d", result, err, refreshes)
	}

	collector.run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("grok: not logged in")
	}
	writeGrokAuth(t, home, grokAuth("revoked", "2026-11-01T05:00:00Z"))
	if _, err := collector.fetchGrok(context.Background()); err == nil || !strings.Contains(err.Error(), "refresh Grok login") {
		t.Fatalf("expected refresh failure: %v", err)
	}
}

func grokBillingJSON(t *testing.T, raw string) grokBilling {
	t.Helper()
	var billing grokBilling
	if err := json.Unmarshal([]byte(raw), &billing); err != nil {
		t.Fatal(err)
	}
	return billing
}
