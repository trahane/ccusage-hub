package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/trahane/ccusage-hub/internal/limits"
	"github.com/trahane/ccusage-hub/internal/model"
)

const laptopID = "11111111-1111-4111-8111-111111111111"
const desktopID = "22222222-2222-4222-8222-222222222222"

func limitsReport(device, account string, at time.Time, used float64) limits.Report {
	reset := at.Add(time.Hour)
	return limits.Report{SchemaVersion: 1, Device: model.Device{ID: device, Name: "Test device", Platform: "test"}, CheckedAt: at, Providers: []limits.ProviderLimits{{Provider: "antigravity", AccountID: account, AccountLabel: "Test account", UpdatedAt: at, Primary: &limits.Window{UsedPercent: used, WindowMinutes: 300, ResetsAt: &reset}, Extra: []limits.ExtraWindow{}}}}
}
func mustLimitsStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "limits.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func putLimits(t *testing.T, s *Store, r limits.Report) {
	t.Helper()
	if err := s.IngestLimits(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}
func readLimits(t *testing.T, s *Store, now time.Time) limits.Response {
	t.Helper()
	r, err := s.Limits(context.Background(), "UTC", now, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLimitsReportersShareQuotaWithoutSummingAndSurviveFailures(t *testing.T) {
	s := mustLimitsStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	putLimits(t, s, limitsReport(laptopID, "shared", now, 10))
	putLimits(t, s, limitsReport(desktopID, "shared", now.Add(time.Second), 12))
	r := readLimits(t, s, now.Add(2*time.Second))
	if len(r.Providers) != 1 || len(r.Accounts) != 1 || r.Providers[0].Primary.UsedPercent != 12 || r.Providers[0].DeviceID != desktopID {
		t.Fatalf("quotas were not selected: %+v", r)
	}
	fail := limitsReport(desktopID, "", now.Add(3*time.Second), 0)
	fail.Providers[0].Primary = nil
	fail.Providers[0].Stale = true
	fail.Providers[0].Error = "not signed in"
	putLimits(t, s, fail)
	r = readLimits(t, s, now.Add(4*time.Second))
	if r.Providers[0].Stale || r.Providers[0].Primary.UsedPercent != 10 || r.Providers[0].DeviceID != laptopID {
		t.Fatalf("failed device poisoned healthy reading: %+v", r.Providers)
	}
}
func TestLimitsGoStaleWithoutRefreshingObservationTime(t *testing.T) {
	s := mustLimitsStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	putLimits(t, s, limitsReport(laptopID, "shared", now, 10))
	r := readLimits(t, s, now.Add(limits.StaleAfter+time.Second))
	p := r.Providers[0]
	if !p.Stale || p.Error == "" || !p.UpdatedAt.Equal(now) || p.Primary.UsedPercent != 10 {
		t.Fatalf("offline cache looked live: %+v", p)
	}
}
func TestLimitsFailuresKeepLastGoodWindowsAndRejectOutOfOrderReports(t *testing.T) {
	s := mustLimitsStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	putLimits(t, s, limitsReport(laptopID, "shared", now, 10))
	fail := limitsReport(laptopID, "", now.Add(3*time.Second), 0)
	fail.Providers[0].Stale = true
	fail.Providers[0].Error = "logged out"
	fail.Providers[0].Primary = nil
	putLimits(t, s, fail)
	putLimits(t, s, limitsReport(laptopID, "shared", now.Add(time.Second), 99))
	r := readLimits(t, s, now.Add(4*time.Second))
	p := r.Providers[0]
	if !p.Stale || p.Primary.UsedPercent != 10 || !p.UpdatedAt.Equal(now) || p.Error != "logged out" {
		t.Fatalf("failure or delayed reading lost cache: %+v", p)
	}
}
func TestLimitsPersistAccountsAndDefaultAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	putLimits(t, s, limitsReport(laptopID, "personal", now, 10))
	putLimits(t, s, limitsReport(desktopID, "work", now.Add(time.Second), 70))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := readLimits(t, s, now.Add(2*time.Second))
	if len(r.Accounts) != 2 || len(r.Providers) != 1 || r.Providers[0].AccountID != "personal" {
		t.Fatalf("account isolation/default lost: %+v", r)
	}
	if err := s.SelectLimitAccount(context.Background(), "antigravity", "work"); err != nil {
		t.Fatal(err)
	}
	r = readLimits(t, s, now.Add(2*time.Second))
	if r.Providers[0].Primary.UsedPercent != 70 {
		t.Fatal("account selection failed")
	}
	if err := s.SelectLimitAccount(context.Background(), "antigravity", "unknown"); err == nil {
		t.Fatal("accepted unknown account")
	}
	filtered, err := s.Limits(context.Background(), "UTC", now, "personal")
	if err != nil || len(filtered.Providers) != 1 || filtered.Providers[0].AccountID != "personal" {
		t.Fatal("account query failed", err)
	}
}
func TestUnidentifiedAccountsNeverMergeAcrossDevices(t *testing.T) {
	s := mustLimitsStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	putLimits(t, s, limitsReport(laptopID, "", now, 10))
	putLimits(t, s, limitsReport(desktopID, "", now.Add(time.Second), 70))
	r := readLimits(t, s, now)
	if len(r.Accounts) != 2 {
		t.Fatal("unidentified accounts merged", r)
	}
}
func TestAccountSwitchInvalidatesOldSourceWithoutMixingWindows(t *testing.T) {
	s := mustLimitsStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	putLimits(t, s, limitsReport(laptopID, "personal", now, 10))
	putLimits(t, s, limitsReport(laptopID, "work", now.Add(time.Second), 70))
	r := readLimits(t, s, now.Add(2*time.Second))
	if len(r.Accounts) != 2 || r.Providers[0].AccountID != "personal" || !r.Providers[0].Stale || r.Providers[0].Primary.UsedPercent != 10 {
		t.Fatal("account switch mixed readings", r)
	}
}
func TestLimitsReadingsCanDecreaseAfterReset(t *testing.T) {
	s := mustLimitsStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	putLimits(t, s, limitsReport(laptopID, "shared", now, 100))
	putLimits(t, s, limitsReport(laptopID, "shared", now.Add(time.Second), 0))
	if p := readLimits(t, s, now.Add(2*time.Second)).Providers[0]; p.Primary.UsedPercent != 0 {
		t.Fatal("applied token high-water semantics to quotas", p)
	}
}
