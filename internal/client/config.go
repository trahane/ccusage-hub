package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trahane/ccusage-hub/internal/model"
)

type Config struct {
	DeviceID       string `json:"deviceId"`
	DeviceName     string `json:"deviceName"`
	ServerURL      string `json:"serverURL"`
	CCUsageCommand string `json:"ccusageCommand"`
	Interval       string `json:"interval"`
	LookbackDays   int    `json:"lookbackDays"`
	BackfillDays   int    `json:"backfillDays"`
	BackfilledAt   string `json:"backfilledAt,omitempty"`
}

func DefaultConfigPath() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "ccusage-hub", "client.json"), nil
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createConfig(path)
	}
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, err
	}
	applyDefaults(&config)
	if err := validateConfig(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func createConfig(path string) (Config, error) {
	id, err := model.NewUUID()
	if err != nil {
		return Config{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return Config{}, err
	}
	config := Config{DeviceID: id, DeviceName: hostname, ServerURL: "http://127.0.0.1:7432", CCUsageCommand: "ccusage", Interval: "10m", LookbackDays: 30, BackfillDays: 366}
	if err := SaveConfig(path, config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func SaveConfig(path string, config Config) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func validateConfig(config Config) error {
	if !model.ValidUUID(config.DeviceID) {
		return errors.New("invalid deviceId in client config")
	}
	if err := model.ValidateDeviceName(config.DeviceName); err != nil {
		return err
	}
	if !strings.HasPrefix(config.ServerURL, "http://") && !strings.HasPrefix(config.ServerURL, "https://") {
		return errors.New("serverURL must use http or https")
	}
	if strings.TrimSpace(config.CCUsageCommand) == "" {
		return errors.New("ccusageCommand is required")
	}
	if config.LookbackDays < 1 || config.LookbackDays > 366 {
		return errors.New("lookbackDays must be between 1 and 366")
	}
	if config.BackfillDays < config.LookbackDays || config.BackfillDays > 366 {
		return errors.New("backfillDays must be between lookbackDays and 366")
	}
	if config.BackfilledAt != "" {
		if _, err := time.Parse(time.RFC3339, config.BackfilledAt); err != nil {
			return errors.New("backfilledAt must be an RFC3339 timestamp")
		}
	}
	return nil
}

func applyDefaults(config *Config) {
	if config.BackfillDays == 0 {
		config.BackfillDays = 366
	}
}
