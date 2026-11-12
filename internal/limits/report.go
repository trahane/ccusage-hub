package limits

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/trahane/ccusage-hub/internal/model"
)

const StaleAfter = 10 * time.Minute

// Report is independent of token/cost snapshots. Only normalized readings and
// account identifiers cross this interface; provider credentials stay local.
type Report struct {
	SchemaVersion int              `json:"schemaVersion"`
	Device        model.Device     `json:"device"`
	CheckedAt     time.Time        `json:"checkedAt"`
	Providers     []ProviderLimits `json:"providers"`
}

func (r Report) Validate(now time.Time) error {
	if r.SchemaVersion != model.SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d", r.SchemaVersion)
	}
	if !model.ValidUUID(r.Device.ID) {
		return fmt.Errorf("device.id must be a UUID")
	}
	if err := model.ValidateDeviceName(r.Device.Name); err != nil {
		return err
	}
	if r.CheckedAt.IsZero() || r.CheckedAt.After(now.Add(2*time.Minute)) {
		return fmt.Errorf("checkedAt must be present and not in the future")
	}
	if len(r.Providers) == 0 || len(r.Providers) > 64 {
		return fmt.Errorf("providers must contain 1-64 readings")
	}
	seen := map[string]bool{}
	for _, p := range r.Providers {
		if !SupportedProvider(p.Provider) {
			return fmt.Errorf("unsupported limits provider %q", p.Provider)
		}
		if len(p.AccountID) > 256 || len(p.AccountLabel) > 256 || len(p.Error) > 512 || len(p.Source) > 64 || len(p.Version) > 64 || len(p.Plan) > 256 {
			return fmt.Errorf("limits metadata is too long")
		}
		key := p.Provider
		if seen[key] {
			return fmt.Errorf("one active account per provider is allowed in each report")
		}
		seen[key] = true
		if p.UpdatedAt.IsZero() || p.UpdatedAt.After(r.CheckedAt.Add(2*time.Minute)) {
			return fmt.Errorf("updatedAt must be present and no later than checkedAt")
		}
		if p.Stale && strings.TrimSpace(p.Error) == "" {
			return fmt.Errorf("failed reading must include an error")
		}
		if !p.Stale && (p.Error != "" || !p.HasWindows()) {
			return fmt.Errorf("successful reading must contain quota windows and no error")
		}
		if len(p.Extra) > 64 {
			return fmt.Errorf("too many quota windows")
		}
		windows := []*Window{p.Primary, p.Secondary}
		for i := range p.Extra {
			if len(p.Extra[i].Title) > 128 {
				return fmt.Errorf("quota title is too long")
			}
			windows = append(windows, &p.Extra[i].Window)
		}
		for _, w := range windows {
			if w != nil && (math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) || w.UsedPercent < 0 || w.UsedPercent > 100 || w.WindowMinutes < 0) {
				return fmt.Errorf("invalid quota window")
			}
		}
	}
	return nil
}

func SupportedProvider(provider string) bool {
	switch provider {
	case "codex", "claude", "antigravity", "grok", "opencode":
		return true
	}
	return false
}
func (p ProviderLimits) HasWindows() bool {
	return p.Primary != nil || p.Secondary != nil || len(p.Extra) > 0
}

// AccountKey is stable across token refreshes. Organization scopes keep personal
// and work quotas separate. Missing identities are scoped to the reporting device.
func AccountKey(identity, organization string) string {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if identity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(identity + "\x00" + strings.ToLower(strings.TrimSpace(organization))))
	return "account:" + hex.EncodeToString(sum[:])
}
