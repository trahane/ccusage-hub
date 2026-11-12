package client

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trahane/ccusage-hub/internal/limits"
)

func testLimitsClient(serverURL string) *Client {
	c := &Client{Config: Config{DeviceID: "11111111-1111-4111-8111-111111111111", DeviceName: "Test Mac", ServerURL: serverURL, LimitsInterval: "20ms", LimitProviders: []string{"antigravity"}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.probeLimits = func(ctx context.Context, p string) limits.ProviderLimits {
		return limits.ProviderLimits{Provider: p, UpdatedAt: time.Now().UTC(), Primary: &limits.Window{UsedPercent: 5, WindowMinutes: 300}}
	}
	return c
}
func TestPushLimitsUsesOnlyLimitsEndpoint(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/limits" {
			t.Errorf("unexpected cost/info request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		var report limits.Report
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
		}
		if len(report.Providers) != 1 || report.Providers[0].Primary.UsedPercent != 5 {
			t.Error("bad report", report)
		}
		requests++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	c := testLimitsClient(server.URL)
	c.Config.CCUsageCommand = "/missing/cost-counter"
	if err := c.PushLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || c.Config.BackfilledAt != "" {
		t.Fatal("limits changed backfill state")
	}
}
func TestLimitsRunWhileCostsAndAnotherProviderAreBlocked(t *testing.T) {
	costsStarted := make(chan struct{})
	limitReports := make(chan struct{}, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/info":
			close(costsStarted)
			<-r.Context().Done()
		case "/api/v1/limits":
			limitReports <- struct{}{}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c := testLimitsClient(server.URL)
	c.Config.LimitProviders = []string{"antigravity", "claude"}
	c.probeLimits = func(ctx context.Context, p string) limits.ProviderLimits {
		if p == "claude" {
			<-ctx.Done()
			return limits.ProviderLimits{Provider: p, UpdatedAt: time.Now().UTC(), Stale: true, Error: "canceled"}
		}
		return limits.ProviderLimits{Provider: p, UpdatedAt: time.Now().UTC(), Primary: &limits.Window{UsedPercent: 5}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, time.Hour) }()
	select {
	case <-costsStarted:
	case <-time.After(time.Second):
		t.Fatal("cost loop did not start")
	}
	for range 2 {
		select {
		case <-limitReports:
		case <-time.After(time.Second):
			t.Fatal("limits blocked on cost collection or another provider")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("workers did not stop on cancellation")
	}
}
func TestLimitSubmissionFailuresRetryIndependently(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := testLimitsClient(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Millisecond)
	defer cancel()
	if err := c.RunLimits(ctx); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 2 {
		t.Fatal("limits job stopped after transport failure")
	}
}
func TestCLIErrorOutputIsNormalizedWithoutLeakingExtraFields(t *testing.T) {
	// Exercise the real CLI -> decoder -> HTTP path, including a nonzero exit.
	command := filepath.Join(t.TempDir(), "codexbar")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s' '[{\"provider\":\"antigravity\",\"credentials\":\"never-forward-this\",\"error\":{\"message\":\"Sign in required\"}}]'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if strings.Contains(string(raw), "never-forward-this") {
			t.Error("raw credentials leaked into normalized report")
		}
		var report limits.Report
		if err := json.Unmarshal(raw, &report); err != nil {
			t.Error(err)
		}
		p := report.Providers[0]
		if !p.Stale || p.Error != "Sign in required" {
			t.Error("lost provider failure", p)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	c := testLimitsClient(server.URL)
	c.probeLimits = nil
	c.Config.CodexBarCommand = command
	if err := c.PushLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUnconfiguredNativeProviderDoesNotPostEmptyLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unconfigured provider sent a report")
		w.WriteHeader(500)
	}))
	defer server.Close()
	c := testLimitsClient(server.URL)
	c.Config.LimitProviders = []string{"opencode"}
	c.probeLimits = func(context.Context, string) limits.ProviderLimits {
		return limits.ProviderLimits{Provider: "opencode", NotConfigured: true}
	}
	if err := c.PushLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
}
