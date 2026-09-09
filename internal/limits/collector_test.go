package limits

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestDecodeProviderLimits(t *testing.T) {
	raw := []byte(`[{"provider":"claude","source":"claude","version":"2.1.207","usage":{"identity":{"loginMethod":"subscription"},"primary":{"usedPercent":100,"windowMinutes":300,"resetsAt":"2026-07-12T01:10:00Z"},"secondary":{"usedPercent":28,"windowMinutes":10080,"resetsAt":"2026-07-13T04:00:00Z"},"extraRateWindows":[],"updatedAt":"2026-07-11T23:29:21Z"}}]`)
	result, err := decode("claude", raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan != "subscription" || result.Primary == nil || result.Primary.UsedPercent != 100 {
		t.Fatalf("unexpected result: %+v", result)
	}
	want := time.Date(2026, 7, 12, 1, 10, 0, 0, time.UTC)
	if result.Primary.ResetsAt == nil || !result.Primary.ResetsAt.Equal(want) {
		t.Fatalf("reset time: %v", result.Primary.ResetsAt)
	}
}

func TestDecodeExtraWindowsAndCredits(t *testing.T) {
	raw := []byte(`[{"provider":"codex","source":"oauth","version":"0.144.1","usage":{"loginMethod":"prolite","primary":{"usedPercent":55,"windowMinutes":300,"resetsAt":"2026-07-11T23:48:37Z"},"secondary":{"usedPercent":26,"windowMinutes":10080,"resetsAt":"2026-07-18T07:34:31Z"},"extraRateWindows":[{"title":"Codex Spark 5-hour","window":{"usedPercent":0,"windowMinutes":300,"resetsAt":"2026-07-12T04:31:05Z"}}],"codexResetCredits":{"availableCount":2},"updatedAt":"2026-07-11T23:31:06Z"}}]`)
	result, err := decode("codex", raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResetCredits != 2 || len(result.Extra) != 1 || result.Extra[0].Title != "Codex Spark 5-hour" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestAntigravityCollectionRetainsQuotaPoolsAfterFailure(t *testing.T) {
	collector := New("codexbar", slog.New(slog.NewTextHandler(io.Discard, nil)))
	calls := 0
	collector.run = func(_ context.Context, command string, args ...string) ([]byte, error) {
		if command != "codexbar" || !reflect.DeepEqual(args, []string{"usage", "--provider", "antigravity", "--source", "auto", "--format", "json", "--no-color"}) {
			t.Fatalf("unexpected command: %s %v", command, args)
		}
		calls++
		if calls == 2 {
			return nil, errors.New("Antigravity unavailable")
		}
		return []byte(`[{"provider":"antigravity","source":"cli","usage":{"identity":{"loginMethod":"Google AI Pro"},"primary":{"usedPercent":12,"windowMinutes":10080,"resetsAt":"2026-09-10T19:22:03Z"},"secondary":{"usedPercent":0,"windowMinutes":300,"resetsAt":"2026-09-09T03:54:32Z"},"extraRateWindows":[{"title":"Gemini 5-hour","window":{"usedPercent":3,"windowMinutes":300,"resetsAt":"2026-09-09T03:54:32Z"}}],"updatedAt":"2026-09-08T23:00:00Z"}}]`), nil
	}
	collector.collect(context.Background(), "antigravity", "auto")
	fresh := collector.Snapshot("UTC").Providers[0]
	if fresh.Stale || fresh.Plan != "Google AI Pro" || fresh.Primary == nil || fresh.Primary.UsedPercent != 12 || fresh.Primary.WindowMinutes != 10080 || fresh.Secondary == nil || fresh.Secondary.UsedPercent != 0 {
		t.Fatalf("unexpected quota pools: %+v", fresh)
	}
	if len(fresh.Extra) != 1 || fresh.Extra[0].Title != "Gemini 5-hour" || fresh.Extra[0].ResetsAt == nil {
		t.Fatalf("lost named model quota: %+v", fresh.Extra)
	}
	collector.collect(context.Background(), "antigravity", "auto")
	stale := collector.Snapshot("UTC").Providers[0]
	if !stale.Stale || stale.Error != "Antigravity unavailable" || !stale.UpdatedAt.Equal(fresh.UpdatedAt) || !reflect.DeepEqual(stale.Primary, fresh.Primary) || !reflect.DeepEqual(stale.Extra, fresh.Extra) {
		t.Fatalf("lost cached quota on failure: %+v", stale)
	}
}

func TestAntigravityMissingPoolsRemainAbsent(t *testing.T) {
	result, err := decode("antigravity", []byte(`[{"provider":"antigravity","usage":{"extraRateWindows":[{"title":"Gemini model","window":{"usedPercent":25}}]}}]`))
	if err != nil || result.Primary != nil || result.Secondary != nil || len(result.Extra) != 1 {
		t.Fatalf("unexpected sparse quotas: %+v, %v", result, err)
	}
}
