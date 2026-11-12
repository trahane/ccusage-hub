package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trahane/ccusage-hub/internal/limits"
)

// A bounded latest-reading table, independent of cumulative token accounting.
// Per-device attempts are separate so a failed reporter cannot poison a healthy one.
func (s *Store) IngestLimits(ctx context.Context, report limits.Report) error {
	now := time.Now().UTC()
	if err := report.Validate(now); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO devices(id,name,platform,client_version,first_seen,last_seen)
 VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,platform=excluded.platform,
 client_version=excluded.client_version,last_seen=excluded.last_seen`, report.Device.ID, strings.TrimSpace(report.Device.Name), report.Device.Platform, report.Device.ClientVersion, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	for _, p := range report.Providers {
		failed := p.Stale || p.Error != ""
		if p.AccountID == "" && failed {
			err := tx.QueryRowContext(ctx, `SELECT account_id FROM limit_sources WHERE device_id=? AND provider=?`, report.Device.ID, p.Provider).Scan(&p.AccountID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if p.AccountID == "" {
			p.AccountID = "device:" + report.Device.ID
		}
		if p.AccountLabel == "" {
			p.AccountLabel = report.Device.Name + " (unidentified account)"
			if !strings.HasPrefix(p.AccountID, "device:") {
				p.AccountLabel = p.AccountID
			}
		}
		p.DeviceID, p.DeviceName, p.CheckedAt, p.ReceivedAt = report.Device.ID, report.Device.Name, report.CheckedAt, now
		var previousCheck int64
		err := tx.QueryRowContext(ctx, `SELECT checked_at FROM limit_sources WHERE device_id=? AND provider=?`, p.DeviceID, p.Provider).Scan(&previousCheck)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if previousCheck >= report.CheckedAt.UnixNano() {
			continue
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO limit_sources(device_id,provider,account_id,checked_at,payload_json) VALUES(?,?,?,?,?)
   ON CONFLICT(device_id,provider) DO UPDATE SET account_id=excluded.account_id,checked_at=excluded.checked_at,payload_json=excluded.payload_json`, p.DeviceID, p.Provider, p.AccountID, p.CheckedAt.UnixNano(), string(raw)); err != nil {
			return err
		}
		if failed {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO limit_observations(device_id,provider,account_id,observed_at,payload_json) VALUES(?,?,?,?,?)
   ON CONFLICT(device_id,provider,account_id) DO UPDATE SET observed_at=excluded.observed_at,payload_json=excluded.payload_json
   WHERE excluded.observed_at > limit_observations.observed_at`, p.DeviceID, p.Provider, p.AccountID, p.UpdatedAt.UnixNano(), string(raw)); err != nil {
			return err
		}
		// Keep the displayed account stable when another device signs into a different account.
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO limit_defaults(provider,account_id) VALUES(?,?)`, p.Provider, p.AccountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SelectLimitAccount(ctx context.Context, provider, accountID string) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM limit_observations WHERE provider=? AND account_id=?`, provider, accountID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no successful reading for that provider/account")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO limit_defaults(provider,account_id) VALUES(?,?) ON CONFLICT(provider) DO UPDATE SET account_id=excluded.account_id`, provider, accountID)
	return err
}

func (s *Store) Limits(ctx context.Context, timezone string, now time.Time, accountFilter string) (limits.Response, error) {
	result := limits.Response{SchemaVersion: 1, Timezone: timezone, Providers: []limits.ProviderLimits{}, Accounts: []limits.ProviderLimits{}}
	rows, err := s.db.QueryContext(ctx, `SELECT o.payload_json,s.payload_json FROM limit_observations o
 JOIN limit_sources s ON s.device_id=o.device_id AND s.provider=o.provider
 UNION ALL SELECT s.payload_json,s.payload_json FROM limit_sources s WHERE NOT EXISTS
 (SELECT 1 FROM limit_observations o WHERE o.device_id=s.device_id AND o.provider=s.provider AND o.account_id=s.account_id)`)
	if err != nil {
		return result, err
	}
	groups := map[string]limits.ProviderLimits{}
	for rows.Next() {
		var observation, source string
		if err := rows.Scan(&observation, &source); err != nil {
			rows.Close()
			return result, err
		}
		var p, check limits.ProviderLimits
		if err := json.Unmarshal([]byte(observation), &p); err != nil {
			rows.Close()
			return result, err
		}
		if err := json.Unmarshal([]byte(source), &check); err != nil {
			rows.Close()
			return result, err
		}
		p.CheckedAt = check.CheckedAt
		switch {
		case check.AccountID != p.AccountID:
			p.Stale = true
			p.Error = "Reporting device switched accounts"
		case check.Stale || check.Error != "":
			p.Stale = true
			p.Error = check.Error
		case now.Sub(p.UpdatedAt) > limits.StaleAfter || now.Sub(check.CheckedAt) > limits.StaleAfter:
			p.Stale = true
			p.Error = "No current limits reading; reporting device may be offline"
		}
		key := p.Provider + "\x00" + p.AccountID
		previous, ok := groups[key]
		if !ok || betterLimit(p, previous) {
			groups[key] = p
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	defaults := map[string]string{}
	rows, err = s.db.QueryContext(ctx, `SELECT provider,account_id FROM limit_defaults`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var p, a string
		if err := rows.Scan(&p, &a); err != nil {
			rows.Close()
			return result, err
		}
		defaults[p] = a
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	selected := map[string]limits.ProviderLimits{}
	for _, p := range groups {
		if accountFilter != "" && p.AccountID != accountFilter {
			continue
		}
		result.Accounts = append(result.Accounts, p)
		if accountFilter == "" && defaults[p.Provider] != "" && defaults[p.Provider] != p.AccountID {
			continue
		}
		previous, ok := selected[p.Provider]
		if !ok || betterLimit(p, previous) {
			selected[p.Provider] = p
		}
	}
	for _, p := range selected {
		result.Providers = append(result.Providers, p)
		if p.UpdatedAt.After(result.UpdatedAt) {
			result.UpdatedAt = p.UpdatedAt
		}
	}
	sort.Slice(result.Providers, func(i, j int) bool { return result.Providers[i].Provider < result.Providers[j].Provider })
	sort.Slice(result.Accounts, func(i, j int) bool {
		a, b := result.Accounts[i], result.Accounts[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.AccountID < b.AccountID
	})
	return result, nil
}

func betterLimit(a, b limits.ProviderLimits) bool {
	if a.HasWindows() != b.HasWindows() {
		return a.HasWindows()
	}
	if a.Stale != b.Stale {
		return !a.Stale
	}
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.DeviceID < b.DeviceID
}
