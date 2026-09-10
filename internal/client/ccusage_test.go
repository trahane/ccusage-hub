package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuildSnapshotFromCCUsage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	fixture, err := os.ReadFile("testdata/ccusage.json")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-ccusage")
	content := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo 'ccusage 20.0.17'
  exit 0
fi
online=false
for arg in "$@"; do
  case "$arg" in
    --offline) echo 'offline pricing leaves newer models unpriced' >&2; exit 1 ;;
    --no-offline) online=true ;;
  esac
done
if [ "$online" != true ]; then
  echo 'online pricing must override ccusage offline configuration' >&2
  exit 1
fi
cat <<'JSON'
` + string(fixture) + "\nJSON\n"

	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	config := Config{DeviceID: "11111111-1111-4111-8111-111111111111", DeviceName: "Fixture Mac", ServerURL: "http://localhost", CCUsageCommand: script, Interval: "10m", LookbackDays: 30}
	snapshot, err := BuildSnapshot(context.Background(), config, "America/Los_Angeles", "test")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CCUsage.Version != "20.0.17" || len(snapshot.Days) != 1 || snapshot.Days[0].Sources[0].CostUSD != "1.23456789" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	model := snapshot.Days[0].Sources[0].Models[0]
	if model.TotalTokens != 625 || model.InputTokens != 100 || model.CacheReadTokens != 500 ||
		model.CacheCreationTokens != 5 || model.OutputTokens != 20 ||
		model.CostUSD != "1.23456789" || model.ModelName != "gpt-test" {
		t.Fatalf("model: %+v", model)
	}
}
