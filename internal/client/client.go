package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/trahane/ccusage-hub/internal/model"
)

type Client struct {
	Config     Config
	ConfigPath string
	Version    string
	HTTP       *http.Client
	Log        *slog.Logger
}

func (c *Client) Push(ctx context.Context) (model.IngestResult, error) {
	timezone, err := c.serverTimezone(ctx)
	if err != nil {
		return model.IngestResult{}, err
	}
	buildCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	snapshotConfig := c.Config
	isBackfill := c.Config.BackfilledAt == ""
	if isBackfill {
		snapshotConfig.LookbackDays = c.Config.BackfillDays
	}
	snapshot, err := BuildSnapshot(buildCtx, snapshotConfig, timezone, c.Version)
	if err != nil {
		return model.IngestResult{}, err
	}
	body, _ := json.Marshal(snapshot)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Config.ServerURL, "/")+"/api/v1/snapshots", bytes.NewReader(body))
	if err != nil {
		return model.IngestResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http().Do(request)
	if err != nil {
		return model.IngestResult{}, err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return model.IngestResult{}, fmt.Errorf("server returned %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	var result model.IngestResult
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if isBackfill {
		c.Config.BackfilledAt = time.Now().UTC().Format(time.RFC3339)
		if err := SaveConfig(c.ConfigPath, c.Config); err != nil {
			return result, fmt.Errorf("snapshot accepted but backfill state could not be saved: %w", err)
		}
		c.Log.Info("initial usage backfill completed", "days", snapshotConfig.LookbackDays)
	}
	c.Log.Info("snapshot submitted", "snapshotId", result.SnapshotID, "status", result.Status, "appliedBuckets", result.AppliedBuckets, "tokenDelta", result.TokenDelta, "costDeltaUSD", result.CostDeltaUSD)
	return result, nil
}

func (c *Client) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("interval must be positive")
	}
	for {
		if _, err := c.Push(ctx); err != nil {
			c.Log.Error("snapshot submission failed", "error", err)
		}
		timer := time.NewTimer(model.Jitter(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (c *Client) Rename(ctx context.Context, name string) error {
	if err := model.ValidateDeviceName(name); err != nil {
		return err
	}
	c.Config.DeviceName = strings.TrimSpace(name)
	if err := SaveConfig(c.ConfigPath, c.Config); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"name": c.Config.DeviceName})
	url := strings.TrimRight(c.Config.ServerURL, "/") + "/api/v1/devices/" + c.Config.DeviceID
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http().Do(request)
	if err != nil {
		c.Log.Warn("name saved locally; server sync deferred", "error", err)
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		c.Log.Warn("name saved locally; server sync deferred", "status", response.Status)
		return nil
	}
	c.Log.Info("client renamed", "deviceId", c.Config.DeviceID, "name", c.Config.DeviceName)
	return nil
}

func (c *Client) serverTimezone(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Config.ServerURL, "/")+"/api/v1/info", nil)
	if err != nil {
		return "", err
	}
	response, err := c.http().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server info returned %s", response.Status)
	}
	var info infoResponse
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		return "", err
	}
	if info.Timezone == "" {
		return "", errors.New("server returned empty timezone")
	}
	return info.Timezone, nil
}
func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}
