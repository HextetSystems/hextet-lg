package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/HextetSystems/hextet-lg/internal/config"
	"github.com/HextetSystems/hextet-lg/internal/obs"
	"github.com/HextetSystems/hextet-lg/internal/version"
	"github.com/HextetSystems/hextet-lg/internal/web"
)

func main() {
	// `hextet-lg version` needs no configuration and no CLI framework.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-version", "--version":
			fmt.Print(version.Get().String())
			return
		}
	}
	configPath := flag.String("config", "/etc/hextet-lg/config.yaml", "path to YAML configuration")
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		// The logger needs the configuration, so this one line is plain.
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(1)
	}
	logger := obs.NewLogger(os.Stderr, cfg.Logging.Level, cfg.Logging.Format)
	slog.SetDefault(logger)
	fatal := func(msg string, err error) {
		logger.Error(msg, "error", err.Error())
		os.Exit(1)
	}
	// Bound process memory from the configured budgets unless the operator
	// set GOMEMLIMIT explicitly.
	if os.Getenv("GOMEMLIMIT") == "" {
		budget := cfg.LookingGlass.MemoryBudget()
		debug.SetMemoryLimit(budget)
		logger.Info("soft memory limit set", "mib", budget>>20, "source", "derived from looking_glass limits; set GOMEMLIMIT to override")
	}
	h, err := web.NewHandler(cfg, web.WithLogger(logger))
	if err != nil {
		fatal("initialize web application", err)
	}
	srv := &http.Server{Addr: cfg.Server.Listen, Handler: h.Routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: cfg.Server.MaxHeaderBytes}

	// The metrics listener is separate from the public one and off by default.
	// It is bound before serving so a bad address fails startup visibly.
	var metricsSrv *http.Server
	if cfg.Metrics.Enabled {
		ln, err := net.Listen("tcp", cfg.Metrics.Listen)
		if err != nil {
			fatal("metrics listener", err)
		}
		metricsSrv = &http.Server{Handler: h.MetricsHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 << 10}
		go func() {
			if err := metricsSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				logger.Error("metrics server stopped", "error", err.Error())
			}
		}()
		if cfg.Metrics.LoopbackOnly() {
			logger.Info("metrics listening", "listen", cfg.Metrics.Listen, "scope", "loopback")
		} else {
			logger.Warn("metrics listening on a non-loopback address; restrict access with network policy and never publish this port", "listen", cfg.Metrics.Listen)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	vi := version.Get()
	logger.Info("listening", "version", vi.Version, "commit", vi.Commit, "site", h.SiteName(), "listen", cfg.Server.Listen, "backends", len(cfg.Backends), "log_level", cfg.Logging.Level, "log_format", cfg.Logging.Format, "metrics", cfg.Metrics.Enabled)
	code := 0
	select {
	case err := <-done:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server stopped", "error", err.Error())
			code = 1
		}
		h.Close()
	case <-ctx.Done():
		logger.Info("signal received", "signal", "terminate")
		if err := h.Shutdown(srv, cfg.Server.ShutdownGrace); err != nil {
			logger.Warn("shutdown grace period expired; remaining connections closed", "error", err.Error())
		}
	}
	if metricsSrv != nil {
		mctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = metricsSrv.Shutdown(mctx)
		cancel()
	}
	if code != 0 {
		os.Exit(code)
	}
}
