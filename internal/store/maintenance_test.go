package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func rowCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestIdenticalObservationsCoalesceAndRebaseUsesLatestCapture(t *testing.T) {
	s := openTestStore(t)
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	for i := 0; i < 100; i++ {
		value := snapshot(fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i), deviceOne, "Mac", base.Add(time.Duration(i)*time.Second), 100, "1", "model")
		ingest(t, s, value)
	}
	if got := rowCount(t, s, "observations"); got != 1 {
		t.Fatalf("100 identical uploads produced %d observations", got)
	}
	// Lower corrections also coalesce without changing the high-water total.
	for i := 100; i < 110; i++ {
		ingest(t, s, snapshot(fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i), deviceOne, "Mac", base.Add(time.Duration(i)*time.Second), 80, "0.8", "model"))
	}
	// A later equal-to-high-water capture must supersede a lower correction.
	latest := snapshot("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", deviceOne, "Mac", base.Add(120*time.Second), 100, "1", "model")
	if result := ingest(t, s, latest); result.TokenDelta != 0 {
		t.Fatalf("identical high-water upload changed totals: %+v", result)
	}
	// A stale, different upload must not become the observation used by rebase.
	ingest(t, s, snapshot("cccccccc-cccc-4ccc-8ccc-cccccccccccc", deviceOne, "Mac", base.Add(115*time.Second), 70, "0.7", "model"))
	if err := s.Rebase(context.Background(), deviceOne, base.Format("2006-01-02"), "codex"); err != nil {
		t.Fatal(err)
	}
	devices, err := s.Devices(context.Background())
	if err != nil || devices[0].TotalTokens != 100 {
		t.Fatalf("rebase selected stale observation: %+v, %v", devices, err)
	}
	// Token totals alone cannot detect a pricing/model correction.
	ingest(t, s, snapshot("dddddddd-dddd-4ddd-8ddd-dddddddddddd", deviceOne, "Mac", base.Add(130*time.Second), 100, "2", "new-model"))
	devices, err = s.Devices(context.Background())
	if err != nil || devices[0].CostUSD != "2" {
		t.Fatalf("lost metadata correction: %+v, %v", devices, err)
	}
}

func TestPrunePreservesTotalsRecentAuditAndInactiveRebase(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := now.Truncate(24 * time.Hour).Add(12 * time.Hour)
	a := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	b := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	c := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	d := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	ingest(t, s, snapshot(a, deviceOne, "Mac", base, 100, "1", "model"))
	ingest(t, s, snapshot(b, deviceOne, "Mac", base.Add(time.Minute), 80, "0.8", "model"))
	ingest(t, s, snapshot(c, deviceOne, "Mac", base.Add(2*time.Minute), 70, "0.7", "model"))
	ingest(t, s, snapshot(d, deviceTwo, "Inactive Mac", base.AddDate(0, 0, -100), 50, "0.5", "model"))
	old := now.Add(-AuditRetention - time.Hour).Format(time.RFC3339Nano)
	for _, table := range []string{"snapshots", "observations"} {
		if _, err := s.db.Exec("UPDATE "+table+" SET received_at=?", old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("UPDATE observations SET received_at=? WHERE snapshot_id=?", now.Format(time.RFC3339Nano), b); err != nil {
		t.Fatal(err)
	}
	before, err := s.Devices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	after, err := s.Devices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("prune changed totals/devices: before=%+v after=%+v", before, after)
	}
	if got := rowCount(t, s, "observations"); got != 3 {
		t.Fatalf("expected recent correction and two latest observations, got %d", got)
	}
	var payloadBytes int
	if err := s.db.QueryRow("SELECT sum(length(payload_gzip)) FROM snapshots").Scan(&payloadBytes); err != nil || payloadBytes != 0 {
		t.Fatalf("expired payloads retained: %d, %v", payloadBytes, err)
	}
	rows, err := s.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("pruning left broken foreign keys")
	}
	rows.Close()
	if err := s.Rebase(ctx, deviceOne, base.Format("2006-01-02"), "codex"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebase(ctx, deviceTwo, base.AddDate(0, 0, -100).Format("2006-01-02"), "codex"); err != nil {
		t.Fatal(err)
	}
	devices, err := s.Devices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range devices {
		want := int64(70)
		if device.ID == deviceTwo {
			want = 50
		}
		if device.TotalTokens != want {
			t.Fatalf("lost rebase data: %+v", device)
		}
	}
	// A second pass is safe after the retained old payloads are already empty.
	if err := s.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceReclaimsDatabaseAndWALWithoutLosingTotals(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i)
		ingest(t, s, snapshot(id, deviceOne, "Mac", now.Add(time.Duration(i)*time.Second), 100, "1", "model"))
		if _, err := s.db.Exec("UPDATE snapshots SET payload_gzip=zeroblob(2097152) WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	wal, err := os.Stat(s.path + "-wal")
	if err != nil || wal.Size() < 16<<20 {
		t.Fatalf("test failed to create a large WAL: %v, %v", wal, err)
	}
	old := now.Add(-AuditRetention - time.Hour).Format(time.RFC3339Nano)
	for _, table := range []string{"snapshots", "observations"} {
		if _, err := s.db.Exec("UPDATE "+table+" SET received_at=?", old); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Maintain(ctx, now); err != nil {
		t.Fatal(err)
	}
	wal, err = os.Stat(s.path + "-wal")
	if err != nil || wal.Size() != 0 {
		t.Fatalf("WAL not truncated: %v, %v", wal, err)
	}
	file, err := os.Stat(s.path)
	if err != nil || file.Size() > 1<<20 {
		t.Fatalf("database not compacted: %v, %v", file, err)
	}
	if got := rowCount(t, s, "snapshots"); got != 1 {
		t.Fatalf("expired unreferenced snapshots retained: %d", got)
	}
	devices, err := s.Devices(ctx)
	if err != nil || devices[0].TotalTokens != 100 || devices[0].CostUSD != "1" {
		t.Fatalf("cleanup changed totals: %+v, %v", devices, err)
	}
}

func TestConnectionSettingsSurviveConnectionReplacement(t *testing.T) {
	s := openTestStore(t)
	s.db.SetMaxIdleConns(0)
	for query, want := range map[string]int{"PRAGMA foreign_keys": 1, "PRAGMA busy_timeout": 5000, "PRAGMA wal_autocheckpoint": 1000, "PRAGMA journal_size_limit": 8388608} {
		var got int
		if err := s.db.QueryRow(query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s after reconnect: got %d, want %d, error %v", query, got, want, err)
		}
	}
}
