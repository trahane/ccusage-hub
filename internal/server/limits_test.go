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
	"strings"
	"testing"
	"time"

	"github.com/trahane/ccusage-hub/internal/limits"
	"github.com/trahane/ccusage-hub/internal/model"
	"github.com/trahane/ccusage-hub/internal/store"
)

func TestLimitsReportingWorksWithoutServerCollectorOrTokenSnapshot(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "report.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Config{Timezone: "America/Los_Angeles"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	now := time.Now().UTC()
	reset := now.Add(5 * time.Hour)
	report := limits.Report{SchemaVersion: 1, Device: model.Device{ID: "11111111-1111-4111-8111-111111111111", Name: "Mac"}, CheckedAt: now, Providers: []limits.ProviderLimits{{Provider: "antigravity", AccountID: "shared", UpdatedAt: now, Primary: &limits.Window{UsedPercent: 7, WindowMinutes: 300, ResetsAt: &reset}, Extra: []limits.ExtraWindow{{Title: "Gemini weekly", Window: limits.Window{UsedPercent: 9, WindowMinutes: 10080}}}}}}
	body, _ := json.Marshal(report)
	for range 2 {
		response, err := http.Post(api.URL+"/api/v1/limits", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 202 {
			t.Fatal("limits POST failed", response.StatusCode)
		}
	}
	response, err := http.Get(api.URL + "/api/v1/limits")
	if err != nil {
		t.Fatal(err)
	}
	var got limits.Response
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got.SchemaVersion != 1 || got.Timezone != "America/Los_Angeles" || len(got.Providers) != 1 || len(got.Accounts) != 1 || got.Providers[0].Primary.UsedPercent != 7 || len(got.Providers[0].Extra) != 1 {
		t.Fatalf("mobile response shape changed: %+v", got)
	}
	response, err = http.Get(api.URL + "/api/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(info), `"limits-reporting"`) {
		t.Fatal("missing reporting capability")
	}
	usage, err := db.Usage(context.Background(), 1, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Totals.TotalTokens != 0 {
		t.Fatal("limits changed token totals")
	}
}
func TestLimitsRejectsInvalidReportsAndUnknownSecretFields(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Config{Timezone: "UTC"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, body := range []string{`{}`, `{"schemaVersion":1,"accessToken":"secret"}`, `{} {}`, strings.Repeat(" ", 257<<10)} {
		request := httptest.NewRequest("POST", "/api/v1/limits", strings.NewReader(body))
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, request)
		if response.Code != 400 {
			t.Fatalf("invalid body accepted: %d", response.Code)
		}
		if strings.Contains(response.Body.String(), "secret") {
			t.Fatal("secret echoed")
		}
	}
}
