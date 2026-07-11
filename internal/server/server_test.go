package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/trahane/ccusage-hub/internal/model"
	"github.com/trahane/ccusage-hub/internal/store"
)

func TestSnapshotAPIAndRename(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Config{Timezone: "America/Los_Angeles", Version: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	snapshot := model.Snapshot{SchemaVersion: 1, SnapshotID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", CapturedAt: time.Now().UTC(), Device: model.Device{ID: "11111111-1111-4111-8111-111111111111", Name: "MacBook", Platform: "darwin-arm64", ClientVersion: "test"}, CCUsage: model.CCUsageInfo{Version: "20", LookbackDays: 30}, Days: []model.DayUsage{{Date: time.Now().In(mustLocation(t, "America/Los_Angeles")).Format("2006-01-02"), Sources: []model.SourceUsage{{Source: "codex", Metrics: model.Metrics{TotalTokens: 10, CostUSD: "0.1"}}}}}}
	body, _ := json.Marshal(snapshot)
	response, err := http.Post(httpServer.URL+"/api/v1/snapshots", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status %d", response.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodPatch, httpServer.URL+"/api/v1/devices/"+snapshot.Device.ID, bytes.NewBufferString(`{"name":"Renamed client"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status %d", response.StatusCode)
	}
	response, err = http.Get(httpServer.URL + "/api/v1/usage?days=7")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var usage store.UsageResponse
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatal(err)
	}
	if usage.Totals.TotalTokens != 10 {
		t.Fatalf("usage total %d", usage.Totals.TotalTokens)
	}
	devices, err := db.Devices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].Name != "Renamed client" {
		t.Fatalf("rename missing: %+v", devices[0])
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return location
}
