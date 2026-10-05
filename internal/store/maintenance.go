package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/trahane/ccusage-hub/internal/model"
)

// AuditRetention is separate from usage history: daily totals are never expired.
const AuditRetention = 7 * 24 * time.Hour

// Coalesce consecutive identical observations, including lower observations used
// by admin rebase. Comparing only against current_usage would lose corrections.
func recordObservation(ctx context.Context, tx *sql.Tx, snapshot model.Snapshot, date string, source model.SourceUsage, cost int64, models string, received time.Time, applied bool, reason string, deltaTokens, deltaCost int64) error {
	var id int64
	var input, output, cacheRead, cacheCreate, total, previousCost int64
	var previousModels, captured string
	err := tx.QueryRowContext(ctx, `SELECT id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,cost_nanos,models_json,captured_at
		FROM observations WHERE device_id=? AND usage_date=? AND source=? ORDER BY captured_at DESC,received_at DESC,id DESC LIMIT 1`,
		snapshot.Device.ID, date, source.Source).Scan(&id, &input, &output, &cacheRead, &cacheCreate, &total, &previousCost, &previousModels, &captured)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && input == source.InputTokens && output == source.OutputTokens && cacheRead == source.CacheReadTokens && cacheCreate == source.CacheCreationTokens && total == source.TotalTokens && previousCost == cost && previousModels == models {
		previousTime, err := time.Parse(time.RFC3339Nano, captured)
		if err != nil {
			return err
		}
		if snapshot.CapturedAt.Before(previousTime) {
			return nil
		}
		// Keep the latest capture for stale checks and rebase, without another row.
		_, err = tx.ExecContext(ctx, `UPDATE observations SET snapshot_id=?,captured_at=?,received_at=? WHERE id=?`,
			snapshot.SnapshotID, snapshot.CapturedAt.UTC().Format(time.RFC3339Nano), received.Format(time.RFC3339Nano), id)
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO observations(snapshot_id,device_id,usage_date,source,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,cost_nanos,models_json,captured_at,received_at,applied,reason,delta_tokens,delta_cost_nanos)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, snapshot.SnapshotID, snapshot.Device.ID, date, source.Source, source.InputTokens, source.OutputTokens,
		source.CacheReadTokens, source.CacheCreationTokens, source.TotalTokens, cost, models, snapshot.CapturedAt.UTC().Format(time.RFC3339Nano),
		received.Format(time.RFC3339Nano), boolInt(applied), reason, deltaTokens, deltaCost)
	return err
}

// Prune drops expired audit history while keeping the newest observation of
// every bucket (including inactive devices) and snapshots referenced by totals.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cutoff := now.UTC().Add(-AuditRetention).Format(time.RFC3339Nano)
	statements := []string{
		`DELETE FROM observations WHERE received_at < ? AND id NOT IN (
			SELECT id FROM (SELECT id,ROW_NUMBER() OVER (
				PARTITION BY device_id,usage_date,source ORDER BY captured_at DESC,received_at DESC,id DESC
			) AS rank FROM observations) WHERE rank=1)`,
		`DELETE FROM snapshots WHERE received_at < ?
			AND id NOT IN (SELECT snapshot_id FROM current_usage)
			AND id NOT IN (SELECT snapshot_id FROM observations)`,
		// Retained snapshot references need only metadata after the audit window.
		`UPDATE snapshots SET payload_gzip=X'',warnings_json='[]' WHERE received_at < ? AND (length(payload_gzip)>0 OR warnings_json!='[]')`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, cutoff); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Checkpoint also truncates the WAL. journal_size_limit alone cannot do this
// while a long-lived connection keeps the journal open.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, frames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("WAL checkpoint blocked by another connection (%d/%d frames)", checkpointed, frames)
	}
	return nil
}

// Maintain reclaims disk once at least 16 MiB and a quarter of the database are
// free. This avoids a full rewrite on each upload or on each maintenance tick.
func (s *Store) Maintain(ctx context.Context, now time.Time) error {
	if err := s.Prune(ctx, now); err != nil {
		return err
	}
	var pages, free, pageSize int64
	for _, query := range []struct {
		sql  string
		dest *int64
	}{
		{"PRAGMA page_count", &pages}, {"PRAGMA freelist_count", &free}, {"PRAGMA page_size", &pageSize},
	} {
		if err := s.db.QueryRowContext(ctx, query.sql).Scan(query.dest); err != nil {
			return err
		}
	}
	if free*pageSize >= 16<<20 && free*4 >= pages {
		if err := s.Compact(ctx); err != nil {
			return err
		}
	}
	return s.Checkpoint(ctx)
}

// Compact is also exposed as an offline admin command for immediate cleanup.
func (s *Store) Compact(ctx context.Context) error {
	if err := s.Checkpoint(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	return s.Checkpoint(ctx)
}
