package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/trahane/ccusage-hub/internal/client"
	"github.com/trahane/ccusage-hub/internal/server"
	"github.com/trahane/ccusage-hub/internal/store"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	if len(os.Args) < 2 {
		return usageError()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "server":
		return runServer(ctx, logger, os.Args[2:])
	case "client":
		return runClient(ctx, logger, os.Args[2:])
	case "admin":
		return runAdmin(ctx, os.Args[2:])
	case "version", "--version", "-version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return usageError()
	}
}

func runServer(ctx context.Context, logger *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := flags.String("listen", env("CCUSAGE_HUB_LISTEN", "0.0.0.0:7432"), "HTTP listen address")
	database := flags.String("database", env("CCUSAGE_HUB_DATABASE", "./data/ccusage-hub.db"), "SQLite database path")
	timezone := flags.String("timezone", env("CCUSAGE_HUB_TIMEZONE", "America/Los_Angeles"), "canonical IANA timezone")
	codexbarCommand := flags.String("codexbar-command", os.Getenv("CCUSAGE_HUB_CODEXBAR_COMMAND"), "optional CodexBar CLI path for subscription limits")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, err := time.LoadLocation(*timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	storage, err := store.Open(*database)
	if err != nil {
		return err
	}
	defer storage.Close()
	return server.New(storage, server.Config{Listen: *listen, Timezone: *timezone, Version: version, CodexBarCommand: *codexbarCommand}, logger).Run(ctx)
}

func runClient(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: ccusage-hub client <run|push|rename>")
	}
	command := args[0]
	flags := flag.NewFlagSet("client "+command, flag.ContinueOnError)
	defaultPath, err := client.DefaultConfigPath()
	if err != nil {
		return err
	}
	configPath := flags.String("config", env("CCUSAGE_HUB_CLIENT_CONFIG", defaultPath), "client config path")
	serverURL := flags.String("server", os.Getenv("CCUSAGE_HUB_SERVER"), "server base URL override")
	ccusageCommand := flags.String("ccusage-command", os.Getenv("CCUSAGE_HUB_CCUSAGE_COMMAND"), "ccusage command override")
	intervalOverride := flags.String("interval", os.Getenv("CCUSAGE_HUB_INTERVAL"), "submission interval override")
	lookbackOverride := flags.Int("lookback-days", envInt("CCUSAGE_HUB_LOOKBACK_DAYS", 0), "lookback override")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	config, err := client.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *serverURL != "" {
		config.ServerURL = *serverURL
	}
	if *ccusageCommand != "" {
		config.CCUsageCommand = *ccusageCommand
	}
	if *lookbackOverride > 0 {
		config.LookbackDays = *lookbackOverride
	}
	c := &client.Client{Config: config, ConfigPath: *configPath, Version: version, Log: logger}
	switch command {
	case "push":
		_, err := c.Push(ctx)
		return err
	case "run":
		intervalText := config.Interval
		if *intervalOverride != "" {
			intervalText = *intervalOverride
		}
		interval, err := time.ParseDuration(intervalText)
		if err != nil {
			return err
		}
		return c.Run(ctx, interval)
	case "rename":
		remaining := flags.Args()
		if len(remaining) != 1 {
			return errors.New("usage: ccusage-hub client rename [flags] \"New device name\"")
		}
		return c.Rename(ctx, remaining[0])
	default:
		return errors.New("unknown client command: " + command)
	}
}

func runAdmin(ctx context.Context, args []string) error {
	if len(args) < 1 || args[0] != "rebase" {
		return errors.New("usage: ccusage-hub admin rebase --device UUID --date YYYY-MM-DD --source NAME")
	}
	flags := flag.NewFlagSet("admin rebase", flag.ContinueOnError)
	database := flags.String("database", env("CCUSAGE_HUB_DATABASE", "./data/ccusage-hub.db"), "SQLite database path")
	device := flags.String("device", "", "device UUID")
	date := flags.String("date", "", "usage date")
	source := flags.String("source", "", "ccusage source")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *device == "" || *date == "" || *source == "" {
		return errors.New("device, date, and source are required")
	}
	storage, err := store.Open(*database)
	if err != nil {
		return err
	}
	defer storage.Close()
	if err := storage.Rebase(ctx, *device, *date, *source); err != nil {
		return err
	}
	fmt.Printf("rebased %s %s %s\n", *device, *date, *source)
	return nil
}

func printUsage() {
	fmt.Print(`ccusage-hub - distributed ccusage aggregation

Commands:
  ccusage-hub server [--listen 0.0.0.0:7432] [--database PATH] [--timezone IANA] [--codexbar-command PATH]
  ccusage-hub client run [--server URL] [--interval 10m]
  ccusage-hub client push [--server URL]
  ccusage-hub client rename [--server URL] "Display name"
  ccusage-hub admin rebase --device UUID --date YYYY-MM-DD --source NAME
  ccusage-hub version
`)
}
func usageError() error { printUsage(); return errors.New("command required") }
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}
func logLevel() slog.Level {
	switch os.Getenv("CCUSAGE_HUB_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
func executableDir() string {
	path, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(path)
}
