package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"sync"
	"time"
)

const commandTimeout = 2 * time.Minute

type Window struct {
	UsedPercent   float64    `json:"usedPercent"`
	WindowMinutes int64      `json:"windowMinutes"`
	ResetsAt      *time.Time `json:"resetsAt"`
}

type ExtraWindow struct {
	Title string `json:"title"`
	Window
}

type ProviderLimits struct {
	Provider     string        `json:"provider"`
	Source       string        `json:"source,omitempty"`
	Version      string        `json:"version,omitempty"`
	Plan         string        `json:"plan,omitempty"`
	Primary      *Window       `json:"primary,omitempty"`
	Secondary    *Window       `json:"secondary,omitempty"`
	Extra        []ExtraWindow `json:"extra"`
	ResetCredits int           `json:"resetCredits,omitempty"`
	UpdatedAt    time.Time     `json:"updatedAt"`
	Stale        bool          `json:"stale,omitempty"`
	Error        string        `json:"error,omitempty"`
}

type Response struct {
	SchemaVersion int              `json:"schemaVersion"`
	Timezone      string           `json:"timezone"`
	Providers     []ProviderLimits `json:"providers"`
	UpdatedAt     time.Time        `json:"updatedAt"`
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type Collector struct {
	command string
	log     *slog.Logger
	run     commandRunner

	mu        sync.RWMutex
	providers map[string]ProviderLimits
}

func New(command string, logger *slog.Logger) *Collector {
	return &Collector{
		command:   command,
		log:       logger,
		run:       runCommand,
		providers: make(map[string]ProviderLimits),
	}
}

func (c *Collector) Start(ctx context.Context) {
	go c.loop(ctx, "codex", "oauth", time.Minute)
	go c.loop(ctx, "claude", "cli", 3*time.Minute)
}

func (c *Collector) loop(ctx context.Context, provider, source string, interval time.Duration) {
	c.collect(ctx, provider, source)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx, provider, source)
		}
	}
}

func (c *Collector) collect(ctx context.Context, provider, source string) {
	started := time.Now().UTC()
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	raw, err := c.run(commandCtx, c.command,
		"usage", "--provider", provider, "--source", source,
		"--format", "json", "--no-color",
	)
	var result ProviderLimits
	if err == nil {
		result, err = decode(provider, raw)
	}
	if err != nil {
		c.mu.RLock()
		previous, hasPrevious := c.providers[provider]
		c.mu.RUnlock()
		if hasPrevious && (previous.Primary != nil || previous.Secondary != nil || len(previous.Extra) > 0) {
			result = previous
			result.Stale = true
			result.Error = err.Error()
		} else {
			result = ProviderLimits{Provider: provider, UpdatedAt: started, Stale: true, Error: err.Error(), Extra: []ExtraWindow{}}
		}
		c.log.Warn("limit collection failed", "provider", provider, "error", err)
	} else {
		result.Stale = false
		result.Error = ""
		c.log.Info("limits collected", "provider", provider, "source", result.Source, "updatedAt", result.UpdatedAt)
	}
	c.mu.Lock()
	c.providers[provider] = result
	c.mu.Unlock()
}

func (c *Collector) Snapshot(timezone string) Response {
	c.mu.RLock()
	providers := make([]ProviderLimits, 0, len(c.providers))
	var updatedAt time.Time
	for _, provider := range c.providers {
		providers = append(providers, provider)
		if provider.UpdatedAt.After(updatedAt) {
			updatedAt = provider.UpdatedAt
		}
	}
	c.mu.RUnlock()
	sort.Slice(providers, func(i, j int) bool { return providers[i].Provider < providers[j].Provider })
	return Response{SchemaVersion: 1, Timezone: timezone, Providers: providers, UpdatedAt: updatedAt}
}

type cliEntry struct {
	Provider string `json:"provider"`
	Source   string `json:"source"`
	Version  string `json:"version"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		LoginMethod string `json:"loginMethod"`
		Identity    struct {
			LoginMethod string `json:"loginMethod"`
		} `json:"identity"`
		Primary          *Window `json:"primary"`
		Secondary        *Window `json:"secondary"`
		ExtraRateWindows []struct {
			Title  string `json:"title"`
			Window Window `json:"window"`
		} `json:"extraRateWindows"`
		CodexResetCredits struct {
			AvailableCount int `json:"availableCount"`
		} `json:"codexResetCredits"`
		UpdatedAt time.Time `json:"updatedAt"`
	} `json:"usage"`
}

func decode(provider string, raw []byte) (ProviderLimits, error) {
	var entries []cliEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return ProviderLimits{}, fmt.Errorf("decode CodexBar output: %w", err)
	}
	if len(entries) == 0 {
		return ProviderLimits{}, errors.New("CodexBar returned no usage entry")
	}
	entry := entries[0]
	if entry.Error != nil {
		return ProviderLimits{}, errors.New(entry.Error.Message)
	}
	if entry.Provider != "" && entry.Provider != provider {
		return ProviderLimits{}, fmt.Errorf("CodexBar returned provider %q", entry.Provider)
	}
	updatedAt := entry.Usage.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	plan := entry.Usage.LoginMethod
	if plan == "" {
		plan = entry.Usage.Identity.LoginMethod
	}
	extra := make([]ExtraWindow, 0, len(entry.Usage.ExtraRateWindows))
	for _, item := range entry.Usage.ExtraRateWindows {
		extra = append(extra, ExtraWindow{Title: item.Title, Window: item.Window})
	}
	return ProviderLimits{
		Provider:     provider,
		Source:       entry.Source,
		Version:      entry.Version,
		Plan:         plan,
		Primary:      entry.Usage.Primary,
		Secondary:    entry.Usage.Secondary,
		Extra:        extra,
		ResetCredits: entry.Usage.CodexResetCredits.AvailableCount,
		UpdatedAt:    updatedAt,
	}, nil
}

func runCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, command, args...).Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && len(exitError.Stderr) > 0 {
			return nil, fmt.Errorf("CodexBar: %s", exitError.Stderr)
		}
		return nil, err
	}
	return output, nil
}
