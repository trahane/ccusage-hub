package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Grok quotas come from the Grok CLI's billing proxy, authorized by the CLI's own
// login in auth.json: the same path as CodexBar's grok-cli-proxy source.
const (
	grokProxyURL        = "https://cli-chat-proxy.grok.com"
	grokOIDCScopePrefix = "https://auth.x.ai::"
	grokLegacyScope     = "https://accounts.x.ai/sign-in"
	// Refresh slightly early so a token cannot expire between reading and using it.
	grokRefreshMargin = 2 * time.Minute
	grokShortWindow   = 24 * 60
)

var (
	errGrokUnauthorized = errors.New("Grok rejected the saved login")
	errGrokNotSignedIn  = fmt.Errorf("Grok is not signed in; run `grok login --device-auth` on the hub host: %w", errNotConfigured)
)

type grokSource struct {
	// home is the Grok CLI's data directory, holding auth.json.
	home string
	// command is the Grok CLI, run to refresh an expired login.
	command string
	baseURL string
	client  *http.Client
	now     func() time.Time
}

func defaultGrokSource() grokSource {
	home := os.Getenv("GROK_HOME")
	if home == "" {
		userHome, _ := os.UserHomeDir()
		home = filepath.Join(userHome, ".grok")
	}
	command := os.Getenv("CCUSAGE_HUB_GROK_COMMAND")
	if command == "" {
		// The Grok installer puts the CLI here; service PATHs often omit it.
		command = filepath.Join(home, "bin", "grok")
		if _, err := os.Stat(command); err != nil {
			command = "grok"
		}
	}
	return grokSource{
		home:    home,
		command: command,
		baseURL: grokProxyURL,
		client:  &http.Client{Timeout: 15 * time.Second},
		now:     time.Now,
	}
}

type grokCredential struct {
	Key       string `json:"key"`
	AuthMode  string `json:"auth_mode"`
	ExpiresAt string `json:"expires_at"`
}

func (c grokCredential) expiresBy(deadline time.Time) bool {
	expiresAt, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
	return err == nil && !expiresAt.After(deadline)
}

func (c *Collector) fetchGrok(ctx context.Context) (ProviderLimits, error) {
	credential, err := readGrokCredential(c.grok.home)
	if err == nil && credential.expiresBy(c.grok.now().Add(grokRefreshMargin)) {
		credential, err = c.refreshGrok(ctx)
	}
	if err != nil {
		return ProviderLimits{}, err
	}
	result, err := c.grokLimits(ctx, credential)
	if errors.Is(err, errGrokUnauthorized) {
		if credential, err = c.refreshGrok(ctx); err != nil {
			return ProviderLimits{}, err
		}
		result, err = c.grokLimits(ctx, credential)
	}
	return result, err
}

// refreshGrok lets the Grok CLI renew its own login, so the hub never writes the
// CLI's credential file. Any authenticated command refreshes; `models` is cheap.
func (c *Collector) refreshGrok(ctx context.Context) (grokCredential, error) {
	if _, err := c.run(ctx, c.grok.command, "models"); err != nil {
		return grokCredential{}, fmt.Errorf("refresh Grok login: %w", err)
	}
	credential, err := readGrokCredential(c.grok.home)
	if err == nil && credential.expiresBy(c.grok.now()) {
		err = errors.New("Grok login expired; run `grok login --device-auth` on the hub host")
	}
	return credential, err
}

func readGrokCredential(home string) (grokCredential, error) {
	raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return grokCredential{}, errGrokNotSignedIn
	}
	if err != nil {
		return grokCredential{}, err
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return grokCredential{}, fmt.Errorf("decode Grok auth.json: %w", err)
	}
	scopes := make([]string, 0, len(entries))
	for scope := range entries {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	var legacy *grokCredential
	for _, scope := range scopes {
		var credential grokCredential
		// API-key logins have no subscription quota; other entries may not be logins at all.
		if json.Unmarshal(entries[scope], &credential) != nil || credential.Key == "" || credential.AuthMode == "api_key" {
			continue
		}
		if strings.HasPrefix(scope, grokOIDCScopePrefix) {
			return credential, nil
		}
		if legacy == nil && (scope == grokLegacyScope || strings.Contains(scope, "/sign-in")) {
			legacy = &credential
		}
	}
	if legacy != nil {
		return *legacy, nil
	}
	return grokCredential{}, errors.New("Grok auth.json has no subscription login; run `grok login --device-auth` on the hub host")
}

type grokBilling struct {
	Config *struct {
		CreditUsagePercent *float64 `json:"creditUsagePercent"`
		CurrentPeriod      *struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"currentPeriod"`
		BillingPeriodStart string `json:"billingPeriodStart"`
		BillingPeriodEnd   string `json:"billingPeriodEnd"`
		SubscriptionTier   string `json:"subscriptionTier"`
	} `json:"config"`
	SubscriptionTier string `json:"subscriptionTier"`
}

func (c *Collector) grokLimits(ctx context.Context, credential grokCredential) (ProviderLimits, error) {
	var billing grokBilling
	if err := c.grokGet(ctx, credential, "/v1/billing?format=credits", &billing); err != nil {
		return ProviderLimits{}, err
	}
	result, err := decodeGrokBilling(billing, c.grok.now())
	if err != nil {
		return ProviderLimits{}, err
	}
	// The plan name lives only in CLI settings; missing it must not fail the quota.
	var settings struct {
		SubscriptionTierDisplay string `json:"subscription_tier_display"`
	}
	if c.grokGet(ctx, credential, "/v1/settings", &settings) == nil {
		if plan := strings.TrimSpace(settings.SubscriptionTierDisplay); plan != "" {
			result.Plan = plan
		}
	}
	return result, nil
}

func (c *Collector) grokGet(ctx context.Context, credential grokCredential, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.grok.baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+credential.Key)
	request.Header.Set("x-xai-token-auth", "xai-grok-cli")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ccusage-hub")
	response, err := c.grok.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return errGrokUnauthorized
	case response.StatusCode != http.StatusOK:
		return fmt.Errorf("Grok %s: HTTP %d: %.200s", path, response.StatusCode, body)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode Grok %s: %w", path, err)
	}
	return nil
}

func decodeGrokBilling(billing grokBilling, now time.Time) (ProviderLimits, error) {
	config := billing.Config
	if config == nil {
		return ProviderLimits{}, errors.New("Grok billing returned no quota")
	}
	// Take start and end from the same period; never mix two billing periods.
	start, end := config.BillingPeriodStart, config.BillingPeriodEnd
	if config.CurrentPeriod != nil && config.CurrentPeriod.End != "" {
		start, end = config.CurrentPeriod.Start, config.CurrentPeriod.End
	}
	periodStart, startErr := time.Parse(time.RFC3339Nano, start)
	periodEnd, endErr := time.Parse(time.RFC3339Nano, end)
	activePeriod := startErr == nil && endErr == nil && periodStart.Before(periodEnd) && !periodStart.After(now)

	var usedPercent float64
	switch {
	case config.CreditUsagePercent != nil && !math.IsNaN(*config.CreditUsagePercent) && !math.IsInf(*config.CreditUsagePercent, 0):
		usedPercent = min(100, max(0, *config.CreditUsagePercent))
	case activePeriod:
		// Grok omits a zero percentage but still sends the period it applies to.
		usedPercent = 0
	default:
		return ProviderLimits{}, errors.New("Grok billing returned no usage percentage")
	}

	window := &Window{UsedPercent: usedPercent}
	if endErr == nil {
		resetsAt := periodEnd.UTC()
		window.ResetsAt = &resetsAt
	}
	if activePeriod {
		window.WindowMinutes = int64(periodEnd.Sub(periodStart) / time.Minute)
	}
	result := ProviderLimits{
		Provider:  "grok",
		Source:    "grok-cli-proxy",
		Plan:      strings.TrimSpace(config.SubscriptionTier),
		Extra:     []ExtraWindow{},
		UpdatedAt: now.UTC(),
	}
	if result.Plan == "" {
		result.Plan = strings.TrimSpace(billing.SubscriptionTier)
	}
	// Grok's quota is weekly or monthly; primary is reserved for a short window.
	if window.WindowMinutes > 0 && window.WindowMinutes < grokShortWindow {
		result.Primary = window
	} else {
		result.Secondary = window
	}
	return result, nil
}
