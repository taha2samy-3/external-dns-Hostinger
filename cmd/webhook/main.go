package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/hostinger"
	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/provider"
	"github.com/taha2samy-3/external-dns-hostinger-webhook/internal/server"
)

// version is set at build time via -ldflags.
var version = "dev"

type config struct {
	token        string
	apiURL       string
	webhookAddr  string
	healthAddr   string
	maxBodySize  int64
	providerOpts provider.Config
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := newLogger()
	slog.SetDefault(log)
	log.Info("starting external-dns-hostinger-webhook", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := hostinger.NewClient(cfg.apiURL, cfg.token, "external-dns-hostinger-webhook/"+version)
	initCtx, cancel := context.WithTimeout(ctx, time.Minute)
	p, err := provider.New(initCtx, client, cfg.providerOpts, log)
	cancel()
	if err != nil {
		return err
	}

	healthMux := http.NewServeMux()
	healthMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	servers := []*http.Server{
		newServer(cfg.webhookAddr, server.Handler(p, cfg.maxBodySize)),
		newServer(cfg.healthAddr, healthMux),
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		log.Info("listening", "addr", s.Addr)
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("server on %s: %w", s.Addr, err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err = <-errCh:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return err
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func loadConfig() (config, error) {
	cfg := config{
		token:       strings.TrimSpace(os.Getenv("HOSTINGER_API_TOKEN")),
		apiURL:      os.Getenv("HOSTINGER_API_URL"),
		webhookAddr: envOr("WEBHOOK_ADDR", "127.0.0.1:8888"),
		healthAddr:  envOr("HEALTH_ADDR", "0.0.0.0:8080"),
	}
	if cfg.token == "" {
		return cfg, errors.New("HOSTINGER_API_TOKEN must be set")
	}
	if f := os.Getenv("DOMAIN_FILTER"); f != "" {
		cfg.providerOpts.Zones = strings.Split(f, ",")
	}

	var err error
	if cfg.providerOpts.DefaultTTL, err = envInt("DEFAULT_TTL", 3600); err != nil {
		return cfg, err
	}
	if cfg.providerOpts.DefaultTTL <= 0 {
		return cfg, fmt.Errorf("DEFAULT_TTL must be positive, got %d", cfg.providerOpts.DefaultTTL)
	}
	if cfg.providerOpts.DryRun, err = envBool("DRY_RUN"); err != nil {
		return cfg, err
	}
	if cfg.providerOpts.RequestTimeout, err = envDuration("REQUEST_TIMEOUT", 12*time.Second); err != nil {
		return cfg, err
	}
	if cfg.providerOpts.ZoneRefreshInterval, err = envDuration("ZONE_REFRESH_INTERVAL", time.Hour); err != nil {
		return cfg, err
	}
	maxBody, err := envInt("MAX_BODY_SIZE", 32<<20)
	if err != nil {
		return cfg, err
	}
	cfg.maxBodySize = int64(maxBody)
	return cfg, nil
}

func newLogger() *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(envOr("LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "text") {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envBool(key string) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
