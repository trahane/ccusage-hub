package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/trahane/ccusage-hub/internal/model"
)

const (
	deviceOne = "11111111-1111-4111-8111-111111111111"
	deviceTwo = "22222222-2222-4222-8222-222222222222"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func snapshot(id, deviceID, name string, captured time.Time, total int64, cost, modelName string) model.Snapshot {
	input := total / 10
	output := total / 20
	cache := total - input - output
	return model.Snapshot{
		SchemaVersion: model.SchemaVersion, SnapshotID: id, CapturedAt: captured,
		Device:  model.Device{ID: deviceID, Name: name, Platform: "test", ClientVersion: "test"},
		CCUsage: model.CCUsageInfo{Version: "20.0.17", LookbackDays: 30},
		Days:    []model.DayUsage{{Date: captured.UTC().Format("2006-01-02"), Sources: []model.SourceUsage{{Source: "codex", Metrics: model.Metrics{InputTokens: input, OutputTokens: output, CacheReadTokens: cache, TotalTokens: total, CostUSD: cost}, Models: []model.ModelUsage{{ModelName: modelName, Metrics: model.Metrics{InputTokens: input, OutputTokens: output, CacheReadTokens: cache, TotalTokens: total, CostUSD: cost}}}}}}},
	}
}

func ingest(t *testing.T, s *Store, value model.Snapshot) model.IngestResult {
	t.Helper()
	raw, _ := json.Marshal(value)
	result, err := s.Ingest(context.Background(), value, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHighWaterAndFleetAggregation(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().UTC().Add(-time.Hour)
	first := ingest(t, s, snapshot("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", deviceOne, "MacBook Pro", base, 100, "1.25", "gpt-a"))
	if first.TokenDelta != 100 || first.CostDeltaUSD != "1.25" {
		t.Fatalf("first delta: %+v", first)
	}
	second := ingest(t, s, snapshot("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", deviceOne, "MacBook Pro", base.Add(5*time.Minute), 130, "1.75", "gpt-b"))
	if second.TokenDelta != 30 || second.CostDeltaUSD != "0.5" {
		t.Fatalf("second delta: %+v", second)
	}
	duplicate := ingest(t, s, snapshot("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", deviceOne, "MacBook Pro", base.Add(5*time.Minute), 130, "1.75", "gpt-b"))
	if duplicate.Status != "duplicate" {
		t.Fatalf("duplicate status: %s", duplicate.Status)
	}
	lower := ingest(t, s, snapshot("cccccccc-cccc-4ccc-8ccc-cccccccccccc", deviceOne, "MacBook Pro", base.Add(10*time.Minute), 120, "1.5", "gpt-c"))
	if lower.Status != "ignored" || lower.TokenDelta != 0 {
		t.Fatalf("lower result: %+v", lower)
	}
	ingest(t, s, snapshot("dddddddd-dddd-4ddd-8ddd-dddddddddddd", deviceTwo, "Mac Mini", base, 50, "0.5", "gpt-a"))

	usage, err := s.Usage(context.Background(), 7, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Totals.TotalTokens != 180 || usage.Totals.CostUSD != "2.25" {
		t.Fatalf("fleet totals: %+v", usage.Totals)
	}
	if len(usage.Days) != 1 || len(usage.Days[0].Sources) != 1 {
		t.Fatalf("unexpected breakdown: %+v", usage.Days)
	}
	models := usage.Days[0].Sources[0].Models
	if len(models) != 2 || models[0].ModelName != "gpt-b" {
		t.Fatalf("authoritative models not replaced atomically: %+v", models)
	}
}

func TestRenameAndRebase(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 10, 12, 30, 0, 0, time.UTC)
	ingest(t, s, snapshot("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", deviceOne, "Old name", base, 100, "1", "gpt-a"))
	ingest(t, s, snapshot("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", deviceOne, "Old name", base.Add(time.Minute), 80, "0.8", "gpt-a"))
	if err := s.RenameDevice(context.Background(), deviceOne, "Studio Mac mini"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebase(context.Background(), deviceOne, "2026-07-10", "codex"); err != nil {
		t.Fatal(err)
	}
	devices, err := s.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Name != "Studio Mac mini" || devices[0].TotalTokens != 80 {
		t.Fatalf("device after rebase: %+v", devices)
	}
}
