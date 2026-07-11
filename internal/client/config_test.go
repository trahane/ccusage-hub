package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigAddsBackfillDefaultToLegacyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	legacy := `{
  "deviceId": "11111111-1111-4111-8111-111111111111",
  "deviceName": "Legacy Mac",
  "serverURL": "http://pibot:7432",
  "ccusageCommand": "ccusage",
  "interval": "10m",
  "lookbackDays": 30
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.BackfillDays != 366 || config.BackfilledAt != "" {
		t.Fatalf("unexpected backfill defaults: %+v", config)
	}
}
