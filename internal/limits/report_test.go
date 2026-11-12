package limits

import (
	"context"
	"testing"
	"time"

	"github.com/trahane/ccusage-hub/internal/model"
)

func TestLimitsValidation(t *testing.T) {
	now := time.Now().UTC()
	valid := func() Report {
		return Report{SchemaVersion: 1, Device: model.Device{ID: "11111111-1111-4111-8111-111111111111", Name: "Mac"}, CheckedAt: now, Providers: []ProviderLimits{{Provider: "codex", UpdatedAt: now, Primary: &Window{UsedPercent: 0}}}}
	}
	for _, test := range []struct {
		name   string
		mutate func(*Report)
	}{
		{"future check", func(r *Report) { r.CheckedAt = now.Add(time.Hour) }},
		{"future sample", func(r *Report) { r.Providers[0].UpdatedAt = now.Add(time.Hour) }},
		{"missing identity", func(r *Report) { r.Device.ID = "bad" }},
		{"negative percent", func(r *Report) { r.Providers[0].Primary.UsedPercent = -1 }},
		{"oversized percent", func(r *Report) { r.Providers[0].Primary.UsedPercent = 101 }},
		{"unsupported", func(r *Report) { r.Providers[0].Provider = "unknown" }},
		{"no quotas", func(r *Report) { r.Providers[0].Primary = nil }},
		{"duplicate provider", func(r *Report) { p := r.Providers[0]; p.AccountID = "other"; r.Providers = append(r.Providers, p) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := valid()
			test.mutate(&r)
			if err := r.Validate(now); err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
	if err := valid().Validate(now); err != nil {
		t.Fatal(err)
	}
}
func TestAccountIdentityIsStableAndScoped(t *testing.T) {
	if AccountKey(" Test@Example.com ", "org") != AccountKey("test@example.com", "org") {
		t.Fatal("identity normalization unstable")
	}
	if AccountKey("test@example.com", "org") == AccountKey("test@example.com", "personal") {
		t.Fatal("organization scopes merged")
	}
	if AccountKey("", "") != "" {
		t.Fatal("fabricated identity")
	}
}
func TestDecodeSkipsUnknownQuotaValuesAndExtractsAccountIdentity(t *testing.T) {
	raw := []byte(`[{"provider":"antigravity","usage":{"identity":{"accountEmail":"test@example.com","accountOrganization":"personal"},"primary":{"usedPercent":100,"usageKnown":false},"secondary":{"usedPercent":0},"extraRateWindows":[{"title":"Unavailable model","window":{"usedPercent":100,"usageKnown":false}}],"updatedAt":"2026-11-12T10:00:00Z"}}]`)
	p, err := decode("antigravity", raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Primary != nil || p.Secondary == nil || len(p.Extra) != 0 || p.AccountID != AccountKey("test@example.com", "personal") {
		t.Fatalf("unknown usage was presented as exhausted: %+v", p)
	}
}

func TestNativeOpenCodeProbeDoesNotRequireCodexBar(t *testing.T) {
	fixture := newOpenCodeFixture(t)
	fixture.authFile(t, `{"opencode-go":{"type":"api","key":"good"}}`)
	collector := fixture.collector("")
	collector.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("native provider invoked CodexBar")
		return nil, nil
	}
	p := collector.Probe(context.Background(), "opencode", "api")
	if p.Stale || p.NotConfigured || p.Primary == nil || p.Source != "opencode-go-api" {
		t.Fatalf("native quotas were lost in reporting probe: %+v", p)
	}
	missing := newOpenCodeFixture(t).collector("").Probe(context.Background(), "opencode", "api")
	if !missing.NotConfigured {
		t.Fatalf("unconfigured native provider should be skipped: %+v", missing)
	}
}
