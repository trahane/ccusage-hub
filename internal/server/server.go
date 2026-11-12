package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trahane/ccusage-hub/internal/limits"
	"github.com/trahane/ccusage-hub/internal/model"
	"github.com/trahane/ccusage-hub/internal/store"
)

const maxSnapshotBytes = 5 << 20

type Config struct {
	Listen          string
	Timezone        string
	Version         string
	CodexBarCommand string
}

type Server struct {
	store  *store.Store
	config Config
	log    *slog.Logger
	limits *limits.Collector
}

func New(storage *store.Store, config Config, logger *slog.Logger) *Server {
	var collector *limits.Collector
	if config.CodexBarCommand != "" {
		collector = limits.New(config.CodexBarCommand, logger)
	}
	return &Server{store: storage, config: config, log: logger, limits: collector}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/v1/info", s.info)
	mux.HandleFunc("GET /api/v1/limits", s.usageLimits)
	mux.HandleFunc("POST /api/v1/limits", s.ingestLimits)
	mux.HandleFunc("POST /api/v1/snapshots", s.ingest)
	mux.HandleFunc("GET /api/v1/usage", s.usage)
	mux.HandleFunc("GET /api/v1/models", s.models)
	mux.HandleFunc("GET /api/v1/devices", s.devices)
	mux.HandleFunc("PATCH /api/v1/devices/{deviceID}", s.renameDevice)
	return s.middleware(mux)
}

func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		s.maintainStorage(ctx)
	}()
	defer func() { cancel(); <-maintenanceDone }()
	if s.limits != nil {
		s.limits.OnCollect = func(p limits.ProviderLimits) {
			report := limits.Report{SchemaVersion: 1, Device: model.Device{ID: "00000000-0000-4000-8000-000000000001", Name: "Hub local collector", Platform: "server", ClientVersion: s.config.Version}, CheckedAt: time.Now().UTC(), Providers: []limits.ProviderLimits{p}}
			if err := s.store.IngestLimits(ctx, report); err != nil {
				s.log.Warn("local limits persistence failed", "error", err)
			}
		}
		s.limits.Start(ctx)
	}
	httpServer := &http.Server{
		Addr:              s.config.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("server listening", "address", s.config.Listen, "timezone", s.config.Timezone, "database", s.store.Path())
		errCh <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) maintainStorage(ctx context.Context) {
	checkpoint := time.NewTicker(time.Minute)
	defer checkpoint.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	run := func(full bool) {
		maintenanceCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		var err error
		if full {
			err = s.store.Maintain(maintenanceCtx, time.Now())
		} else {
			err = s.store.Checkpoint(maintenanceCtx)
		}
		if err != nil && ctx.Err() == nil {
			s.log.Warn("storage maintenance failed", "prune", full, "error", err)
		} else if full && err == nil {
			s.log.Info("storage maintenance completed", "auditRetentionDays", 7)
		}
	}
	run(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-checkpoint.C:
			run(false)
		case <-prune.C:
			run(true)
		}
	}
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,OPTIONS")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("http request", "method", r.Method, "path", r.URL.Path, "durationMs", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.config.Version, "database": "ok", "time": time.Now().UTC()})
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	capabilities := []string{"snapshots", "devices", "models", "costs", "rename", "high-water", "limits", "limits-reporting"}
	writeJSON(w, http.StatusOK, map[string]any{"version": s.config.Version, "schemaVersion": model.SchemaVersion, "timezone": s.config.Timezone, "capabilities": capabilities})
}

func (s *Server) usageLimits(w http.ResponseWriter, r *http.Request) {
	report, err := s.store.Limits(r.Context(), s.config.Timezone, time.Now().UTC(), r.URL.Query().Get("accountId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "limits storage unavailable")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) ingestLimits(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var report limits.Report
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		writeError(w, http.StatusBadRequest, "invalid limits report")
		return
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "expected one limits report")
		return
	}
	if err := report.Validate(time.Now().UTC()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.IngestLimits(r.Context(), report); err != nil {
		s.log.Error("limits ingestion failed", "error", err)
		writeError(w, http.StatusInternalServerError, "limits storage unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSnapshotBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "snapshot exceeds 5 MiB")
		return
	}
	var snapshot model.Snapshot
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot: "+err.Error())
		return
	}
	result, err := s.store.Ingest(r.Context(), snapshot, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("snapshot processed", "snapshotId", snapshot.SnapshotID, "deviceId", snapshot.Device.ID, "deviceName", snapshot.Device.Name, "status", result.Status, "appliedBuckets", result.AppliedBuckets, "ignoredBuckets", result.IgnoredBuckets, "tokenDelta", result.TokenDelta, "costDeltaUSD", result.CostDeltaUSD, "warnings", len(result.Warnings))
	status := http.StatusAccepted
	if result.Status == "duplicate" {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	days, err := parseDays(r, 7)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	usage, err := s.store.Usage(r.Context(), days, s.config.Timezone)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	days, err := parseDays(r, 30)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := s.store.Models(r.Context(), days, s.config.Timezone, r.URL.Query().Get("source"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "source": r.URL.Query().Get("source"), "models": items})
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.Devices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": items})
}

func (s *Server) renameDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid rename request")
		return
	}
	id := r.PathValue("deviceID")
	if err := s.store.RenameDevice(r.Context(), id, body.Name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "device not found")
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	s.log.Info("device renamed", "deviceId", id, "name", strings.TrimSpace(body.Name))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": strings.TrimSpace(body.Name)})
}

func parseDays(r *http.Request, defaultValue int) (int, error) {
	value := r.URL.Query().Get("days")
	if value == "" {
		return defaultValue, nil
	}
	days, err := strconv.Atoi(value)
	if err != nil || days < 1 || days > 366 {
		return 0, fmt.Errorf("days must be between 1 and 366")
	}
	return days, nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"status": status, "message": message}})
}
