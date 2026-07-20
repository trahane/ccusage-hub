package limits

import (
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
