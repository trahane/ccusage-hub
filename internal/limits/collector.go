package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const commandTimeout = 2 * time.Minute

// Native providers can be absent on hosts without their local login.
var errNotConfigured = errors.New("provider is not configured on this reporting host")

type Window struct {
	UsedPercent   float64    `json:"usedPercent"`
	WindowMinutes int64      `json:"windowMinutes"`
	UsageKnown    *bool      `json:"usageKnown,omitempty"`
	ResetsAt      *time.Time `json:"resetsAt"`
}

type ExtraWindow struct {
	Title string `json:"title"`
	Window
}

type ProviderLimits struct {
	Provider      string        `json:"provider"`
	NotConfigured bool          `json:"-"`
	AccountID     string        `json:"accountId,omitempty"`
	AccountLabel  string        `json:"accountLabel,omitempty"`
	DeviceID      string        `json:"deviceId,omitempty"`
	DeviceName    string        `json:"deviceName,omitempty"`
	CheckedAt     time.Time     `json:"checkedAt,omitempty"`
	ReceivedAt    time.Time     `json:"receivedAt,omitempty"`
	Source        string        `json:"source,omitempty"`
	Version       string        `json:"version,omitempty"`
	Plan          string        `json:"plan,omitempty"`
	Primary       *Window       `json:"primary,omitempty"`
	Secondary     *Window       `json:"secondary,omitempty"`
	Extra         []ExtraWindow `json:"extra"`
	ResetCredits  int           `json:"resetCredits,omitempty"`
	UpdatedAt     time.Time     `json:"updatedAt"`
	Stale         bool          `json:"stale,omitempty"`
	Error         string        `json:"error,omitempty"`
}

type Response struct {
	SchemaVersion int              `json:"schemaVersion"`
	Timezone      string           `json:"timezone"`
	Providers     []ProviderLimits `json:"providers"`
	UpdatedAt     time.Time        `json:"updatedAt"`
	Accounts      []ProviderLimits `json:"accounts,omitempty"`
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type Collector struct {
	command   string
	log       *slog.Logger
	run       commandRunner
	OnCollect func(ProviderLimits)
	grok      grokSource
	opencode  openCodeSource

	mu        sync.RWMutex
	providers map[string]ProviderLimits
}

func New(command string, logger *slog.Logger) *Collector {
	return &Collector{
		command:   command,
		log:       logger,
		run:       runCommand,
		grok:      defaultGrokSource(),
		opencode:  defaultOpenCodeSource(),
		providers: make(map[string]ProviderLimits),
	}
}

func (c *Collector) Start(ctx context.Context) {
	go c.loop(ctx, "codex", "oauth", time.Minute)
	go c.loop(ctx, "claude", "cli", 3*time.Minute)
	go c.loop(ctx, "antigravity", "auto", 3*time.Minute)
	go c.loop(ctx, "grok", "cli-proxy", 3*time.Minute)
	go c.loop(ctx, "opencode", "api", 3*time.Minute)
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
	result := c.Probe(ctx, provider, source)
	if result.NotConfigured {
		c.mu.Lock()
		delete(c.providers, provider)
		c.mu.Unlock()
		return
	}
	if result.Stale {
		c.mu.RLock()
		previous, hasPrevious := c.providers[provider]
		c.mu.RUnlock()
		if hasPrevious && previous.HasWindows() {
			previous.Stale, previous.Error = true, result.Error
			result = previous
		}
		c.log.Warn("limit collection failed", "provider", provider, "error", result.Error)
	} else {
		c.log.Info("limits collected", "provider", provider, "source", result.Source, "updatedAt", result.UpdatedAt)
	}
	if c.OnCollect != nil {
		c.OnCollect(result)
	}

	c.mu.Lock()
	c.providers[provider] = result
	c.mu.Unlock()
}

// Probe performs one bounded check without a shared cache or token-counting work.
func (c *Collector) Probe(ctx context.Context, provider, source string) ProviderLimits {
	started := time.Now().UTC()
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	result, err := c.fetch(commandCtx, provider, source)
	if errors.Is(err, errNotConfigured) {
		return ProviderLimits{Provider: provider, NotConfigured: true}
	}

	if err == nil && !result.HasWindows() {
		err = errors.New("Provider returned no quota windows")
	}
	if err != nil {
		message := err.Error()
		if len(message) > 512 {
			message = message[:512]
		}
		return ProviderLimits{Provider: provider, Source: source, UpdatedAt: started, Stale: true, Error: message, Extra: []ExtraWindow{}}
	}
	return result
}

// fetch keeps native Grok/OpenCode sources available to both local and distributed collectors.
func (c *Collector) fetch(ctx context.Context, provider, source string) (ProviderLimits, error) {
	switch provider {
	case "grok":
		return c.fetchGrok(ctx)
	case "opencode":
		return c.fetchOpenCode(ctx)
	}
	raw, err := c.run(ctx, c.command, "usage", "--provider", provider, "--source", source, "--format", "json", "--no-color")
	if len(raw) > 0 {
		parsed, parseErr := decode(provider, raw)
		if err == nil || parseErr != nil {
			return parsed, parseErr
		}
	}
	return ProviderLimits{}, err
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
			LoginMethod         string `json:"loginMethod"`
			AccountEmail        string `json:"accountEmail"`
			AccountOrganization string `json:"accountOrganization"`
			AccountID           string `json:"accountId"`
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
	if len(raw) > 256<<10 {
		return ProviderLimits{}, errors.New("CodexBar limits output exceeds 256 KiB")
	}
	var entries []cliEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return ProviderLimits{}, fmt.Errorf("decode CodexBar output: %w", err)
	}
	if len(entries) != 1 {
		return ProviderLimits{}, errors.New("CodexBar must return exactly one active-account usage entry")
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
	for _, w := range []**Window{&entry.Usage.Primary, &entry.Usage.Secondary} {
		if *w != nil && (*w).UsageKnown != nil && !*(*w).UsageKnown {
			*w = nil
		}
	}
	extra := make([]ExtraWindow, 0, len(entry.Usage.ExtraRateWindows))
	for _, item := range entry.Usage.ExtraRateWindows {
		if item.Window.UsageKnown == nil || *item.Window.UsageKnown {
			extra = append(extra, ExtraWindow{Title: item.Title, Window: item.Window})
		}
	}
	return ProviderLimits{
		Provider:     provider,
		AccountID:    AccountKey(firstNonempty(entry.Usage.Identity.AccountID, entry.Usage.Identity.AccountEmail), entry.Usage.Identity.AccountOrganization),
		AccountLabel: entry.Usage.Identity.AccountEmail,
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
	cmd := exec.CommandContext(ctx, command, args...)
	cleanup := containCommand(cmd)
	defer cleanup()
	// Bound waiting on pipes inherited by grandchildren after cancellation.
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && len(exitError.Stderr) > 0 {
			return output, fmt.Errorf("%s command failed (exit %d)", filepath.Base(command), exitError.ExitCode())
		}
		return output, err
	}
	return output, nil
}

func firstNonempty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
