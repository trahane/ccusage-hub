package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const SchemaVersion = 1

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type Snapshot struct {
	SchemaVersion int         `json:"schemaVersion"`
	SnapshotID    string      `json:"snapshotId"`
	CapturedAt    time.Time   `json:"capturedAt"`
	Device        Device      `json:"device"`
	CCUsage       CCUsageInfo `json:"ccusage"`
	Days          []DayUsage  `json:"days"`
}

type Device struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Platform      string `json:"platform"`
	ClientVersion string `json:"clientVersion"`
}

type CCUsageInfo struct {
	Version      string `json:"version"`
	LookbackDays int    `json:"lookbackDays"`
}

type DayUsage struct {
	Date    string        `json:"date"`
	Sources []SourceUsage `json:"sources"`
}

type Metrics struct {
	InputTokens         int64  `json:"inputTokens"`
	OutputTokens        int64  `json:"outputTokens"`
	CacheReadTokens     int64  `json:"cacheReadTokens"`
	CacheCreationTokens int64  `json:"cacheCreationTokens"`
	TotalTokens         int64  `json:"totalTokens"`
	CostUSD             string `json:"costUSD"`
}

type SourceUsage struct {
	Source string `json:"source"`
	Metrics
	Models []ModelUsage `json:"models"`
}

type ModelUsage struct {
	ModelName string `json:"modelName"`
	Metrics
}

type IngestResult struct {
	SnapshotID     string    `json:"snapshotId"`
	Status         string    `json:"status"`
	ReceivedAt     time.Time `json:"receivedAt"`
	AppliedBuckets int       `json:"appliedBuckets"`
	IgnoredBuckets int       `json:"ignoredBuckets"`
	Warnings       []string  `json:"warnings,omitempty"`
	TokenDelta     int64     `json:"tokenDelta"`
	CostDeltaUSD   string    `json:"costDeltaUSD"`
}

func (s *Snapshot) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d", s.SchemaVersion)
	}
	if !ValidUUID(s.SnapshotID) || !ValidUUID(s.Device.ID) {
		return errors.New("snapshotId and device.id must be UUIDs")
	}
	if s.CapturedAt.IsZero() {
		return errors.New("capturedAt is required")
	}
	if err := ValidateDeviceName(s.Device.Name); err != nil {
		return err
	}
	if s.CCUsage.LookbackDays < 1 || s.CCUsage.LookbackDays > 366 {
		return errors.New("ccusage.lookbackDays must be between 1 and 366")
	}
	if len(s.Days) > 366 {
		return errors.New("too many daily rows")
	}
	seen := make(map[string]struct{})
	for _, day := range s.Days {
		if !datePattern.MatchString(day.Date) {
			return fmt.Errorf("invalid date %q", day.Date)
		}
		if _, err := time.Parse("2006-01-02", day.Date); err != nil {
			return fmt.Errorf("invalid date %q", day.Date)
		}
		for _, source := range day.Sources {
			key := day.Date + "\x00" + source.Source
			if source.Source == "" || len(source.Source) > 64 {
				return errors.New("source must contain 1-64 characters")
			}
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate source %q for %s", source.Source, day.Date)
			}
			seen[key] = struct{}{}
			if err := source.Metrics.Validate(); err != nil {
				return fmt.Errorf("%s/%s: %w", day.Date, source.Source, err)
			}
			if len(source.Models) > 512 {
				return errors.New("too many model rows")
			}
			for _, model := range source.Models {
				if strings.TrimSpace(model.ModelName) == "" || len(model.ModelName) > 256 {
					return errors.New("modelName must contain 1-256 characters")
				}
				if err := model.Metrics.Validate(); err != nil {
					return fmt.Errorf("%s/%s/%s: %w", day.Date, source.Source, model.ModelName, err)
				}
			}
		}
	}
	return nil
}

func (m Metrics) Validate() error {
	if m.InputTokens < 0 || m.OutputTokens < 0 || m.CacheReadTokens < 0 || m.CacheCreationTokens < 0 || m.TotalTokens < 0 {
		return errors.New("token counters cannot be negative")
	}
	if _, err := ParseUSDNanos(m.CostUSD); err != nil {
		return fmt.Errorf("invalid costUSD: %w", err)
	}
	return nil
}

func ValidateDeviceName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" || len([]rune(n)) > 64 {
		return errors.New("device name must contain 1-64 characters")
	}
	return nil
}

func ParseUSDNanos(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if strings.HasPrefix(value, "-") {
		return 0, errors.New("cost cannot be negative")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errors.New("invalid decimal")
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, errors.New("invalid decimal")
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	frac += strings.Repeat("0", 9-len(frac))
	fracValue := int64(0)
	if frac != "" {
		fracValue, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, errors.New("invalid decimal")
		}
	}
	if whole > (1<<63-1-fracValue)/1_000_000_000 {
		return 0, errors.New("cost is too large")
	}
	return whole*1_000_000_000 + fracValue, nil
}

func FormatUSDNanos(value int64) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	whole := value / 1_000_000_000
	frac := value % 1_000_000_000
	if frac == 0 {
		return fmt.Sprintf("%s%d", sign, whole)
	}
	return fmt.Sprintf("%s%d.%s", sign, whole, strings.TrimRight(fmt.Sprintf("%09d", frac), "0"))
}

func NewUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16])), nil
}

func ValidUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil
}

func Jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	span := int64(base / 10)
	if span == 0 {
		return base
	}
	n, err := rand.Int(rand.Reader, big.NewInt(span*2+1))
	if err != nil {
		return base
	}
	return base - time.Duration(span) + time.Duration(n.Int64())
}
