package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/trahane/ccusage-hub/internal/limits"
	"github.com/trahane/ccusage-hub/internal/model"
)

// PushLimits does not read token logs, request a timezone, or change backfill state.
func (c *Client) PushLimits(ctx context.Context) error {
	var wg sync.WaitGroup
	errors := make(chan error, len(c.Config.LimitProviders))
	for _, provider := range c.Config.LimitProviders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.pushProviderLimits(ctx, provider); err != nil {
				errors <- err
			}
		}()
	}
	wg.Wait()
	close(errors)
	var messages []string
	for err := range errors {
		messages = append(messages, err.Error())
	}
	if len(messages) > 0 {
		return fmt.Errorf("limits reporting: %s", strings.Join(messages, "; "))
	}
	return nil
}

func (c *Client) pushProviderLimits(ctx context.Context, provider string) error {
	var p limits.ProviderLimits
	if c.probeLimits != nil {
		p = c.probeLimits(ctx, provider)
	} else {
		source := "auto"
		if provider == "codex" {
			source = "oauth"
		}
		// Preserve CLI compatibility on headless hosts and avoid browser-cookie prompts.
		if provider == "claude" {
			source = "cli"
		}
		p = limits.New(c.Config.CodexBarCommand, c.Log).Probe(ctx, provider, source)
	}
	if p.AccountID == "" && c.Config.LimitAccounts[provider] != "" {
		p.AccountLabel = c.Config.LimitAccounts[provider]
		p.AccountID = limits.AccountKey("configured:"+p.AccountLabel, "")
	}
	report := limits.Report{SchemaVersion: model.SchemaVersion, Device: model.Device{ID: c.Config.DeviceID, Name: c.Config.DeviceName, Platform: runtime.GOOS + "-" + runtime.GOARCH, ClientVersion: c.Version}, CheckedAt: time.Now().UTC(), Providers: []limits.ProviderLimits{p}}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, strings.TrimRight(c.Config.ServerURL, "/")+"/api/v1/limits", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%s limits submission returned HTTP %d", provider, response.StatusCode)
	}
	c.Log.Info("limits submitted", "provider", provider, "stale", p.Stale, "observedAt", p.UpdatedAt)
	return nil
}

func (c *Client) runLimitsJobs(ctx context.Context, interval time.Duration) {
	var wg sync.WaitGroup
	for _, provider := range c.Config.LimitProviders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runEvery(ctx, interval, func() {
				if err := c.pushProviderLimits(ctx, provider); err != nil {
					c.Log.Error("limits submission failed", "provider", provider, "error", err)
				}
			})
		}()
	}
	wg.Wait()
}

func runEvery(ctx context.Context, interval time.Duration, job func()) {
	for ctx.Err() == nil {
		job()
		timer := time.NewTimer(model.Jitter(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *Client) RunLimits(ctx context.Context) error {
	interval, err := time.ParseDuration(c.Config.LimitsInterval)
	if err != nil || interval <= 0 {
		return fmt.Errorf("limitsInterval must be a positive duration")
	}
	c.runLimitsJobs(ctx, interval)
	return nil
}
