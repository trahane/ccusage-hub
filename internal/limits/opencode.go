package limits

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// OpenCode Go quotas come from OpenCode's usage API, authorized by the API key the
// OpenCode CLI stores locally: the same API t3code and CodexBar's API source use.
const (
	openCodeUsagePath     = "/zen/go/v1/usage"
	openCodeConsoleServer = "https://opencode.ai/console"
)

var errOpenCodeNotConfigured = fmt.Errorf("no OpenCode Go subscription key on the hub host: %w", errNotConfigured)

type openCodeSource struct {
	// dataDir is OpenCode's data directory, holding auth.json and its database.
	dataDir  string
	database string
	envKey   string
	baseURL  string
	client   *http.Client
	now      func() time.Time
}

func defaultOpenCodeSource() openCodeSource {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	dataDir := filepath.Join(dataHome, "opencode")
	database := os.Getenv("OPENCODE_DB")
	if database == "" {
		database = "opencode.db"
	}
	if !filepath.IsAbs(database) {
		database = filepath.Join(dataDir, database)
	}
	return openCodeSource{
		dataDir:  dataDir,
		database: database,
		envKey:   strings.TrimSpace(os.Getenv("OPENCODE_API_KEY")),
		baseURL:  "https://opencode.ai",
		client:   &http.Client{Timeout: 15 * time.Second},
		now:      time.Now,
	}
}

func (c *Collector) fetchOpenCode(ctx context.Context) (ProviderLimits, error) {
	source := c.opencode
	keys := source.keys(ctx)
	if len(keys) == 0 {
		return ProviderLimits{}, errOpenCodeNotConfigured
	}
	var rejected error
	for _, key := range keys {
		status, body, err := source.get(ctx, key)
		if err != nil {
			return ProviderLimits{}, err
		}
		switch status {
		case http.StatusOK:
			return decodeOpenCodeUsage(body, source.now())
		case http.StatusUnauthorized:
			rejected = fmt.Errorf("OpenCode rejected the API key: %s", openCodeErrorMessage(body))
		case http.StatusForbidden:
			// A valid OpenCode key without a Go subscription; try the next key.
		default:
			return ProviderLimits{}, fmt.Errorf("OpenCode usage: HTTP %d: %s", status, openCodeErrorMessage(body))
		}
	}
	if rejected != nil {
		return ProviderLimits{}, rejected
	}
	return ProviderLimits{}, errOpenCodeNotConfigured
}

// keys lists candidate API keys in OpenCode's own precedence: OpenCode 2 keeps
// current keys in its database and imports auth.json only once, and auth.json
// overrides the environment.
func (s openCodeSource) keys(ctx context.Context) []string {
	var keys []string
	for _, integration := range []string{"opencode", "opencode-go"} {
		keys = append(keys, s.storedKey(ctx, integration))
	}
	authKey := s.authFileKey()
	if authKey == "" {
		authKey = s.envKey
	}
	keys = append(keys, authKey)
	unique := keys[:0]
	seen := map[string]bool{"": true}
	for _, key := range keys {
		if !seen[key] {
			seen[key] = true
			unique = append(unique, key)
		}
	}
	return unique
}

// storedKey reads the selected OpenCode 2 credential. It selects before decoding so
// a self-hosted or OAuth connection never falls through to another stored account;
// rows with a NULL active flag predate account selection.
func (s openCodeSource) storedKey(ctx context.Context, integration string) string {
	if _, err := os.Stat(s.database); err != nil {
		return ""
	}
	dsn := (&url.URL{Scheme: "file", Path: s.database}).String() + "?mode=ro&_pragma=busy_timeout(100)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return ""
	}
	defer db.Close()
	var raw string
	err = db.QueryRowContext(ctx, `SELECT value FROM credential
		WHERE integration_id = ? AND active IS NOT 0
		ORDER BY active DESC, time_created DESC, id DESC LIMIT 1`, integration).Scan(&raw)
	if err != nil {
		return ""
	}
	var stored struct {
		Type     string `json:"type"`
		Key      string `json:"key"`
		Metadata *struct {
			Server *string `json:"server"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(raw), &stored) != nil || stored.Type != "key" {
		return ""
	}
	if stored.Metadata != nil && stored.Metadata.Server != nil {
		server, err := url.Parse(*stored.Metadata.Server)
		if err != nil || server.Scheme+"://"+server.Host+strings.TrimRight(server.Path, "/") != openCodeConsoleServer {
			return ""
		}
	}
	return strings.TrimSpace(stored.Key)
}

func (s openCodeSource) authFileKey() string {
	raw, err := os.ReadFile(filepath.Join(s.dataDir, "auth.json"))
	if err != nil {
		return ""
	}
	var auth struct {
		Go *struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		} `json:"opencode-go"`
	}
	if json.Unmarshal(raw, &auth) != nil || auth.Go == nil || auth.Go.Type != "api" {
		return ""
	}
	return strings.TrimSpace(auth.Go.Key)
}

func (s openCodeSource) get(ctx context.Context, key string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+openCodeUsagePath, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ccusage-hub")
	response, err := s.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, body, err
}

func openCodeErrorMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return fmt.Sprintf("%.200s", body)
}

type openCodeWindow struct {
	Percent      *float64 `json:"percent"`
	UsagePercent *float64 `json:"usagePercent"`
	ResetsAt     string   `json:"resetsAt"`
	ResetInSec   *int64   `json:"resetInSec"`
}

func (w *openCodeWindow) window(minutes int64, now time.Time) (*Window, bool) {
	if w == nil {
		return nil, false
	}
	percent := w.Percent
	if percent == nil {
		percent = w.UsagePercent
	}
	if percent == nil {
		return nil, false
	}
	window := &Window{UsedPercent: min(100, max(0, *percent)), WindowMinutes: minutes}
	if resetsAt, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
		resetsAt = resetsAt.UTC()
		window.ResetsAt = &resetsAt
	} else if w.ResetInSec != nil {
		resetsAt := now.Add(time.Duration(*w.ResetInSec) * time.Second).UTC()
		window.ResetsAt = &resetsAt
	}
	return window, true
}

func decodeOpenCodeUsage(raw []byte, now time.Time) (ProviderLimits, error) {
	var response struct {
		Usage *struct {
			Rolling *openCodeWindow `json:"rolling"`
			Weekly  *openCodeWindow `json:"weekly"`
			Monthly *openCodeWindow `json:"monthly"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return ProviderLimits{}, fmt.Errorf("decode OpenCode usage: %w", err)
	}
	if response.Usage == nil {
		return ProviderLimits{}, errors.New("OpenCode usage returned no quota")
	}
	rolling, hasRolling := response.Usage.Rolling.window(5*60, now)
	if !hasRolling {
		return ProviderLimits{}, errors.New("OpenCode usage returned no rolling quota")
	}
	weekly, _ := response.Usage.Weekly.window(7*24*60, now)
	result := ProviderLimits{
		Provider:  "opencode",
		Source:    "opencode-go-api",
		Plan:      "Go",
		Primary:   rolling,
		Secondary: weekly,
		Extra:     []ExtraWindow{},
		UpdatedAt: now.UTC(),
	}
	// Monthly periods follow the billing calendar, so their length is left unknown.
	if monthly, ok := response.Usage.Monthly.window(0, now); ok {
		result.Extra = append(result.Extra, ExtraWindow{Title: "Monthly", Window: *monthly})
	}
	return result, nil
}
