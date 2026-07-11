package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/trahane/ccusage-hub/internal/model"
)

type Store struct {
	db   *sql.DB
	path string
}

type currentBucket struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
	CostNanos           int64
	CapturedAt          time.Time
}

type UsageResponse struct {
	Timezone  string         `json:"timezone"`
	From      string         `json:"from"`
	To        string         `json:"to"`
	UpdatedAt time.Time      `json:"updatedAt"`
	Totals    Aggregate      `json:"totals"`
	Days      []DayAggregate `json:"days"`
}

type Aggregate struct {
	InputTokens         int64  `json:"inputTokens"`
	OutputTokens        int64  `json:"outputTokens"`
	CacheReadTokens     int64  `json:"cacheReadTokens"`
	CacheCreationTokens int64  `json:"cacheCreationTokens"`
	TotalTokens         int64  `json:"totalTokens"`
	CostNanos           int64  `json:"-"`
	CostUSD             string `json:"costUSD"`
}

type DayAggregate struct {
	Date string `json:"date"`
	Aggregate
	Sources []SourceAggregate `json:"sources"`
}

type SourceAggregate struct {
	Source string `json:"source"`
	Aggregate
	Models  []ModelAggregate  `json:"models"`
	Devices []DeviceAggregate `json:"devices"`
}

type ModelAggregate struct {
	ModelName string `json:"modelName"`
	Aggregate
}

type DeviceAggregate struct {
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	Aggregate
}

type DeviceSummary struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Platform      string    `json:"platform"`
	ClientVersion string    `json:"clientVersion"`
	FirstSeen     time.Time `json:"firstSeen"`
	LastSeen      time.Time `json:"lastSeen"`
	Aggregate
}

type ModelSummary struct {
	Source string `json:"source"`
	ModelAggregate
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS devices (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			platform TEXT NOT NULL,
			client_version TEXT NOT NULL,
			first_seen TEXT NOT NULL,
			last_seen TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			id TEXT PRIMARY KEY,
			device_id TEXT NOT NULL REFERENCES devices(id),
			captured_at TEXT NOT NULL,
			received_at TEXT NOT NULL,
			ccusage_version TEXT NOT NULL,
			lookback_days INTEGER NOT NULL,
			payload_gzip BLOB NOT NULL,
			status TEXT NOT NULL,
			applied_buckets INTEGER NOT NULL,
			ignored_buckets INTEGER NOT NULL,
			warnings_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS observations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			snapshot_id TEXT NOT NULL REFERENCES snapshots(id),
			device_id TEXT NOT NULL,
			usage_date TEXT NOT NULL,
			source TEXT NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			cache_read_tokens INTEGER NOT NULL,
			cache_creation_tokens INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			cost_nanos INTEGER NOT NULL,
			models_json TEXT NOT NULL,
			captured_at TEXT NOT NULL,
			received_at TEXT NOT NULL,
			applied INTEGER NOT NULL,
			reason TEXT NOT NULL,
			delta_tokens INTEGER NOT NULL,
			delta_cost_nanos INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS current_usage (
			device_id TEXT NOT NULL REFERENCES devices(id),
			usage_date TEXT NOT NULL,
			source TEXT NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			cache_read_tokens INTEGER NOT NULL,
			cache_creation_tokens INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			cost_nanos INTEGER NOT NULL,
			models_json TEXT NOT NULL,
			snapshot_id TEXT NOT NULL REFERENCES snapshots(id),
			captured_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(device_id, usage_date, source)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_current_date ON current_usage(usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_observations_bucket ON observations(device_id, usage_date, source, captured_at)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_device ON snapshots(device_id, received_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("database migration: %w", err)
		}
	}
	return nil
}

func (s *Store) Ingest(ctx context.Context, snapshot model.Snapshot, raw []byte) (model.IngestResult, error) {
	if err := snapshot.Validate(); err != nil {
		return model.IngestResult{}, err
	}
	receivedAt := time.Now().UTC()
	result := model.IngestResult{SnapshotID: snapshot.SnapshotID, ReceivedAt: receivedAt, Status: "applied", CostDeltaUSD: "0"}

	compressed, err := compress(raw)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	var duplicate string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM snapshots WHERE id = ?`, snapshot.SnapshotID).Scan(&duplicate); err == nil {
		result.Status = "duplicate"
		return result, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO devices(id,name,platform,client_version,first_seen,last_seen)
		VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,platform=excluded.platform,
		client_version=excluded.client_version,last_seen=excluded.last_seen`, snapshot.Device.ID, strings.TrimSpace(snapshot.Device.Name), snapshot.Device.Platform,
		snapshot.Device.ClientVersion, receivedAt.Format(time.RFC3339Nano), receivedAt.Format(time.RFC3339Nano)); err != nil {
		return result, err
	}

	warnings := snapshotWarnings(snapshot)
	warningsJSON, _ := json.Marshal(warnings)
	if _, err := tx.ExecContext(ctx, `INSERT INTO snapshots(id,device_id,captured_at,received_at,ccusage_version,lookback_days,payload_gzip,status,applied_buckets,ignored_buckets,warnings_json)
		VALUES(?,?,?,?,?,?,?,?,0,0,?)`, snapshot.SnapshotID, snapshot.Device.ID, snapshot.CapturedAt.UTC().Format(time.RFC3339Nano), receivedAt.Format(time.RFC3339Nano),
		snapshot.CCUsage.Version, snapshot.CCUsage.LookbackDays, compressed, "processing", string(warningsJSON)); err != nil {
		return result, err
	}

	var costDelta int64
	for _, day := range snapshot.Days {
		for _, source := range day.Sources {
			costNanos, _ := model.ParseUSDNanos(source.CostUSD)
			modelsJSON, err := encodeModels(source.Models)
			if err != nil {
				return result, err
			}
			current, found, err := readCurrent(ctx, tx, snapshot.Device.ID, day.Date, source.Source)
			if err != nil {
				return result, err
			}
			apply, reason := !found, "first-observation"
			if found {
				switch {
				case source.TotalTokens > current.TotalTokens:
					apply, reason = true, "higher-high-water"
				case source.TotalTokens == current.TotalTokens && snapshot.CapturedAt.After(current.CapturedAt):
					apply, reason = true, "equal-newer-metadata"
				default:
					apply, reason = false, "lower-or-stale"
				}
			}
			deltaTokens, deltaCost := int64(0), int64(0)
			if apply {
				deltaTokens = source.TotalTokens - current.TotalTokens
				deltaCost = costNanos - current.CostNanos
				if _, err := tx.ExecContext(ctx, `INSERT INTO current_usage(device_id,usage_date,source,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,cost_nanos,models_json,snapshot_id,captured_at,updated_at)
					VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(device_id,usage_date,source) DO UPDATE SET input_tokens=excluded.input_tokens,
					output_tokens=excluded.output_tokens,cache_read_tokens=excluded.cache_read_tokens,cache_creation_tokens=excluded.cache_creation_tokens,
					total_tokens=excluded.total_tokens,cost_nanos=excluded.cost_nanos,models_json=excluded.models_json,snapshot_id=excluded.snapshot_id,
					captured_at=excluded.captured_at,updated_at=excluded.updated_at`, snapshot.Device.ID, day.Date, source.Source, source.InputTokens,
					source.OutputTokens, source.CacheReadTokens, source.CacheCreationTokens, source.TotalTokens, costNanos, modelsJSON, snapshot.SnapshotID,
					snapshot.CapturedAt.UTC().Format(time.RFC3339Nano), receivedAt.Format(time.RFC3339Nano)); err != nil {
					return result, err
				}
				result.AppliedBuckets++
				result.TokenDelta += deltaTokens
				costDelta += deltaCost
			} else {
				result.IgnoredBuckets++
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO observations(snapshot_id,device_id,usage_date,source,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,cost_nanos,models_json,captured_at,received_at,applied,reason,delta_tokens,delta_cost_nanos)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, snapshot.SnapshotID, snapshot.Device.ID, day.Date, source.Source, source.InputTokens, source.OutputTokens,
				source.CacheReadTokens, source.CacheCreationTokens, source.TotalTokens, costNanos, modelsJSON, snapshot.CapturedAt.UTC().Format(time.RFC3339Nano),
				receivedAt.Format(time.RFC3339Nano), boolInt(apply), reason, deltaTokens, deltaCost); err != nil {
				return result, err
			}
		}
	}
	if result.AppliedBuckets == 0 {
		result.Status = "ignored"
	} else if result.IgnoredBuckets > 0 {
		result.Status = "partial"
	}
	result.Warnings = warnings
	result.CostDeltaUSD = model.FormatUSDNanos(costDelta)
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET status=?,applied_buckets=?,ignored_buckets=? WHERE id=?`, result.Status, result.AppliedBuckets, result.IgnoredBuckets, snapshot.SnapshotID); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func readCurrent(ctx context.Context, tx *sql.Tx, deviceID, date, source string) (currentBucket, bool, error) {
	var c currentBucket
	var captured string
	err := tx.QueryRowContext(ctx, `SELECT input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,cost_nanos,captured_at
		FROM current_usage WHERE device_id=? AND usage_date=? AND source=?`, deviceID, date, source).Scan(&c.InputTokens, &c.OutputTokens,
		&c.CacheReadTokens, &c.CacheCreationTokens, &c.TotalTokens, &c.CostNanos, &captured)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	c.CapturedAt, _ = time.Parse(time.RFC3339Nano, captured)
	return c, true, nil
}

func snapshotWarnings(snapshot model.Snapshot) []string {
	var warnings []string
	for _, day := range snapshot.Days {
		for _, source := range day.Sources {
			var modelTokens int64
			var modelCost int64
			for _, item := range source.Models {
				modelTokens += item.TotalTokens
				cost, _ := model.ParseUSDNanos(item.CostUSD)
				modelCost += cost
			}
			sourceCost, _ := model.ParseUSDNanos(source.CostUSD)
			if len(source.Models) > 0 && modelTokens != source.TotalTokens {
				warnings = append(warnings, fmt.Sprintf("%s/%s model tokens sum to %d, source reports %d", day.Date, source.Source, modelTokens, source.TotalTokens))
			}
			if len(source.Models) > 0 && modelCost != sourceCost {
				warnings = append(warnings, fmt.Sprintf("%s/%s model costs sum to %s, source reports %s", day.Date, source.Source, model.FormatUSDNanos(modelCost), source.CostUSD))
			}
		}
	}
	return warnings
}

func encodeModels(models []model.ModelUsage) (string, error) {
	type storedModel struct {
		ModelName           string `json:"modelName"`
		InputTokens         int64  `json:"inputTokens"`
		OutputTokens        int64  `json:"outputTokens"`
		CacheReadTokens     int64  `json:"cacheReadTokens"`
		CacheCreationTokens int64  `json:"cacheCreationTokens"`
		TotalTokens         int64  `json:"totalTokens"`
		CostNanos           int64  `json:"costNanos"`
	}
	stored := make([]storedModel, 0, len(models))
	for _, item := range models {
		cost, err := model.ParseUSDNanos(item.CostUSD)
		if err != nil {
			return "", err
		}
		stored = append(stored, storedModel{item.ModelName, item.InputTokens, item.OutputTokens, item.CacheReadTokens, item.CacheCreationTokens, item.TotalTokens, cost})
	}
	b, err := json.Marshal(stored)
	return string(b), err
}

func decodeModels(raw string) ([]model.ModelUsage, error) {
	var stored []struct {
		ModelName           string `json:"modelName"`
		InputTokens         int64  `json:"inputTokens"`
		OutputTokens        int64  `json:"outputTokens"`
		CacheReadTokens     int64  `json:"cacheReadTokens"`
		CacheCreationTokens int64  `json:"cacheCreationTokens"`
		TotalTokens         int64  `json:"totalTokens"`
		CostNanos           int64  `json:"costNanos"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, err
	}
	models := make([]model.ModelUsage, 0, len(stored))
	for _, item := range stored {
		models = append(models, model.ModelUsage{ModelName: item.ModelName, Metrics: model.Metrics{InputTokens: item.InputTokens, OutputTokens: item.OutputTokens,
			CacheReadTokens: item.CacheReadTokens, CacheCreationTokens: item.CacheCreationTokens, TotalTokens: item.TotalTokens, CostUSD: model.FormatUSDNanos(item.CostNanos)}})
	}
	return models, nil
}

func (s *Store) Usage(ctx context.Context, days int, timezone string) (UsageResponse, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return UsageResponse{}, err
	}
	if days < 1 || days > 366 {
		return UsageResponse{}, errors.New("days must be between 1 and 366")
	}
	now := time.Now().In(loc)
	to := now.Format("2006-01-02")
	from := now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `SELECT c.usage_date,c.source,c.input_tokens,c.output_tokens,c.cache_read_tokens,c.cache_creation_tokens,c.total_tokens,c.cost_nanos,c.models_json,d.id,d.name
		FROM current_usage c JOIN devices d ON d.id=c.device_id WHERE c.usage_date BETWEEN ? AND ? ORDER BY c.usage_date,c.source,d.name`, from, to)
	if err != nil {
		return UsageResponse{}, err
	}
	defer rows.Close()

	response := UsageResponse{Timezone: timezone, From: from, To: to, UpdatedAt: time.Now().UTC(), Days: make([]DayAggregate, 0)}
	dayMap := make(map[string]*DayAggregate)
	type sourceMaps struct {
		models  map[string]*ModelAggregate
		devices map[string]*DeviceAggregate
	}
	mapIndex := make(map[string]sourceMaps)
	for rows.Next() {
		var date, source, modelsJSON, deviceID, deviceName string
		var metrics Aggregate
		if err := rows.Scan(&date, &source, &metrics.InputTokens, &metrics.OutputTokens, &metrics.CacheReadTokens, &metrics.CacheCreationTokens,
			&metrics.TotalTokens, &metrics.CostNanos, &modelsJSON, &deviceID, &deviceName); err != nil {
			return response, err
		}
		day := dayMap[date]
		if day == nil {
			day = &DayAggregate{Date: date}
			dayMap[date] = day
		}
		addAggregate(&day.Aggregate, metrics)
		addAggregate(&response.Totals, metrics)
		var sourceAgg *SourceAggregate
		for i := range day.Sources {
			if day.Sources[i].Source == source {
				sourceAgg = &day.Sources[i]
				break
			}
		}
		if sourceAgg == nil {
			day.Sources = append(day.Sources, SourceAggregate{Source: source})
			sourceAgg = &day.Sources[len(day.Sources)-1]
			mapIndex[date+"\x00"+source] = sourceMaps{models: make(map[string]*ModelAggregate), devices: make(map[string]*DeviceAggregate)}
		}
		addAggregate(&sourceAgg.Aggregate, metrics)
		maps := mapIndex[date+"\x00"+source]
		device := maps.devices[deviceID]
		if device == nil {
			device = &DeviceAggregate{DeviceID: deviceID, DeviceName: deviceName}
			maps.devices[deviceID] = device
		}
		addAggregate(&device.Aggregate, metrics)
		models, err := decodeModels(modelsJSON)
		if err != nil {
			return response, err
		}
		for _, item := range models {
			m := maps.models[item.ModelName]
			if m == nil {
				m = &ModelAggregate{ModelName: item.ModelName}
				maps.models[item.ModelName] = m
			}
			cost, _ := model.ParseUSDNanos(item.CostUSD)
			addAggregate(&m.Aggregate, Aggregate{InputTokens: item.InputTokens, OutputTokens: item.OutputTokens, CacheReadTokens: item.CacheReadTokens, CacheCreationTokens: item.CacheCreationTokens, TotalTokens: item.TotalTokens, CostNanos: cost})
		}
	}
	if err := rows.Err(); err != nil {
		return response, err
	}
	for date, day := range dayMap {
		finalizeAggregate(&day.Aggregate)
		for i := range day.Sources {
			source := &day.Sources[i]
			maps := mapIndex[date+"\x00"+source.Source]
			for _, m := range maps.models {
				finalizeAggregate(&m.Aggregate)
				source.Models = append(source.Models, *m)
			}
			for _, d := range maps.devices {
				finalizeAggregate(&d.Aggregate)
				source.Devices = append(source.Devices, *d)
			}
			sort.Slice(source.Models, func(i, j int) bool { return source.Models[i].TotalTokens > source.Models[j].TotalTokens })
			sort.Slice(source.Devices, func(i, j int) bool { return source.Devices[i].DeviceName < source.Devices[j].DeviceName })
			finalizeAggregate(&source.Aggregate)
		}
		sort.Slice(day.Sources, func(i, j int) bool { return day.Sources[i].Source < day.Sources[j].Source })
		response.Days = append(response.Days, *day)
	}
	sort.Slice(response.Days, func(i, j int) bool { return response.Days[i].Date < response.Days[j].Date })
	finalizeAggregate(&response.Totals)
	return response, nil
}

func (s *Store) Models(ctx context.Context, days int, timezone, sourceFilter string) ([]ModelSummary, error) {
	usage, err := s.Usage(ctx, days, timezone)
	if err != nil {
		return nil, err
	}
	items := make(map[string]*ModelSummary)
	for _, day := range usage.Days {
		for _, source := range day.Sources {
			if sourceFilter != "" && source.Source != sourceFilter {
				continue
			}
			for _, modelAgg := range source.Models {
				key := source.Source + "\x00" + modelAgg.ModelName
				item := items[key]
				if item == nil {
					item = &ModelSummary{Source: source.Source, ModelAggregate: ModelAggregate{ModelName: modelAgg.ModelName}}
					items[key] = item
				}
				addAggregate(&item.Aggregate, modelAgg.Aggregate)
			}
		}
	}
	result := make([]ModelSummary, 0, len(items))
	for _, item := range items {
		finalizeAggregate(&item.Aggregate)
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TotalTokens > result[j].TotalTokens })
	return result, nil
}

func (s *Store) Devices(ctx context.Context) ([]DeviceSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.name,d.platform,d.client_version,d.first_seen,d.last_seen,
		COALESCE(SUM(c.input_tokens),0),COALESCE(SUM(c.output_tokens),0),COALESCE(SUM(c.cache_read_tokens),0),COALESCE(SUM(c.cache_creation_tokens),0),COALESCE(SUM(c.total_tokens),0),COALESCE(SUM(c.cost_nanos),0)
		FROM devices d LEFT JOIN current_usage c ON c.device_id=d.id GROUP BY d.id ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := make([]DeviceSummary, 0)
	for rows.Next() {
		var d DeviceSummary
		var first, last string
		if err := rows.Scan(&d.ID, &d.Name, &d.Platform, &d.ClientVersion, &first, &last, &d.InputTokens, &d.OutputTokens, &d.CacheReadTokens, &d.CacheCreationTokens, &d.TotalTokens, &d.CostNanos); err != nil {
			return nil, err
		}
		d.FirstSeen, _ = time.Parse(time.RFC3339Nano, first)
		d.LastSeen, _ = time.Parse(time.RFC3339Nano, last)
		finalizeAggregate(&d.Aggregate)
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Store) RenameDevice(ctx context.Context, id, name string) error {
	if !model.ValidUUID(id) {
		return errors.New("invalid device UUID")
	}
	if err := model.ValidateDeviceName(name); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE devices SET name=? WHERE id=?`, strings.TrimSpace(name), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) Rebase(ctx context.Context, deviceID, date, source string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var input, output, cacheRead, cacheCreate, total, cost int64
	var modelsJSON, snapshotID, captured, received string
	err = tx.QueryRowContext(ctx, `SELECT input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,cost_nanos,models_json,snapshot_id,captured_at,received_at
		FROM observations WHERE device_id=? AND usage_date=? AND source=? ORDER BY captured_at DESC,received_at DESC LIMIT 1`, deviceID, date, source).Scan(&input, &output, &cacheRead, &cacheCreate, &total, &cost, &modelsJSON, &snapshotID, &captured, &received)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO current_usage(device_id,usage_date,source,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,total_tokens,cost_nanos,models_json,snapshot_id,captured_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(device_id,usage_date,source) DO UPDATE SET input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,cache_read_tokens=excluded.cache_read_tokens,
		cache_creation_tokens=excluded.cache_creation_tokens,total_tokens=excluded.total_tokens,cost_nanos=excluded.cost_nanos,models_json=excluded.models_json,snapshot_id=excluded.snapshot_id,captured_at=excluded.captured_at,updated_at=excluded.updated_at`, deviceID, date, source, input, output, cacheRead, cacheCreate, total, cost, modelsJSON, snapshotID, captured, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func addAggregate(dst *Aggregate, src Aggregate) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.TotalTokens += src.TotalTokens
	dst.CostNanos += src.CostNanos
}
func finalizeAggregate(a *Aggregate) { a.CostUSD = model.FormatUSDNanos(a.CostNanos) }
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func compress(data []byte) ([]byte, error) {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
func Decompress(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
