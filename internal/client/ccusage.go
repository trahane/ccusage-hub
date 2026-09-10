package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/trahane/ccusage-hub/internal/model"
)

type infoResponse struct {
	Timezone string `json:"timezone"`
}

type ccusageReport struct {
	Daily []struct {
		Period string `json:"period"`
		Agents []struct {
			Agent               string      `json:"agent"`
			InputTokens         int64       `json:"inputTokens"`
			OutputTokens        int64       `json:"outputTokens"`
			CacheReadTokens     int64       `json:"cacheReadTokens"`
			CacheCreationTokens int64       `json:"cacheCreationTokens"`
			TotalTokens         int64       `json:"totalTokens"`
			TotalCost           json.Number `json:"totalCost"`
			Models              []struct {
				ModelName           string      `json:"modelName"`
				InputTokens         int64       `json:"inputTokens"`
				OutputTokens        int64       `json:"outputTokens"`
				CacheReadTokens     int64       `json:"cacheReadTokens"`
				CacheCreationTokens int64       `json:"cacheCreationTokens"`
				Cost                json.Number `json:"cost"`
			} `json:"modelBreakdowns"`
		} `json:"agents"`
	} `json:"daily"`
}

func BuildSnapshot(ctx context.Context, config Config, timezone, version string) (model.Snapshot, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return model.Snapshot{}, fmt.Errorf("server timezone: %w", err)
	}
	captured := time.Now().UTC()
	start := captured.In(location).AddDate(0, 0, -(config.LookbackDays - 1)).Format("2006-01-02")
	parts := strings.Fields(config.CCUsageCommand)
	if len(parts) == 0 {
		return model.Snapshot{}, errors.New("empty ccusage command")
	}
	args := append(parts[1:], "daily", "--since", start, "--timezone", timezone, "--by-agent", "--json", "--no-offline")
	command := exec.CommandContext(ctx, parts[0], args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return model.Snapshot{}, fmt.Errorf("ccusage failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	var report ccusageReport
	if err := decoder.Decode(&report); err != nil {
		return model.Snapshot{}, fmt.Errorf("parse ccusage output: %w", err)
	}
	id, err := model.NewUUID()
	if err != nil {
		return model.Snapshot{}, err
	}
	snapshot := model.Snapshot{SchemaVersion: model.SchemaVersion, SnapshotID: id, CapturedAt: captured, Device: model.Device{ID: config.DeviceID, Name: config.DeviceName, Platform: runtime.GOOS + "-" + runtime.GOARCH, ClientVersion: version}, CCUsage: model.CCUsageInfo{Version: ccusageVersion(ctx, parts), LookbackDays: config.LookbackDays}}
	for _, day := range report.Daily {
		item := model.DayUsage{Date: day.Period}
		for _, agent := range day.Agents {
			source := model.SourceUsage{Source: agent.Agent, Metrics: model.Metrics{InputTokens: agent.InputTokens, OutputTokens: agent.OutputTokens, CacheReadTokens: agent.CacheReadTokens, CacheCreationTokens: agent.CacheCreationTokens, TotalTokens: agent.TotalTokens, CostUSD: numberString(agent.TotalCost)}}
			for _, entry := range agent.Models {
				total := entry.InputTokens + entry.OutputTokens + entry.CacheReadTokens + entry.CacheCreationTokens
				source.Models = append(source.Models, model.ModelUsage{ModelName: entry.ModelName, Metrics: model.Metrics{InputTokens: entry.InputTokens, OutputTokens: entry.OutputTokens, CacheReadTokens: entry.CacheReadTokens, CacheCreationTokens: entry.CacheCreationTokens, TotalTokens: total, CostUSD: numberString(entry.Cost)}})
			}
			item.Sources = append(item.Sources, source)
		}
		snapshot.Days = append(snapshot.Days, item)
	}
	if err := snapshot.Validate(); err != nil {
		return model.Snapshot{}, fmt.Errorf("normalized snapshot invalid: %w", err)
	}
	return snapshot, nil
}

func ccusageVersion(ctx context.Context, parts []string) string {
	args := append(append([]string{}, parts[1:]...), "--version")
	command := exec.CommandContext(ctx, parts[0], args...)
	output, err := command.Output()
	if err != nil {
		return "unknown"
	}
	value := strings.TrimSpace(string(output))
	value = strings.TrimPrefix(value, "ccusage ")
	return value
}

func numberString(value json.Number) string {
	if value == "" {
		return "0"
	}
	raw := string(value)
	if strings.ContainsAny(raw, "eE") {
		f, err := strconv.ParseFloat(raw, 64)
		if err == nil {
			return strconv.FormatFloat(f, 'f', 9, 64)
		}
	}
	return raw
}
