package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/admin"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/bridge"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/config"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/metrics"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/vsockdial"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	var showVersion bool
	var checkConfig bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&checkConfig, "check-config", false, "validate configuration and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("tcp-to-vsock-gateway version=%s commit=%s build_time=%s go=%s\n", version, commit, buildTime, config.GoVersion())
		return
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(2)
	}
	if checkConfig {
		fmt.Println("configuration ok")
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	m := metrics.New()
	dialer := vsockdial.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var adminServer *admin.Server
	if cfg.MetricsAddr != "" {
		prober := admin.NewVsockMetricsProber(dialer, cfg.VsockCID, cfg.VsockMetricsPort, cfg.ConnectTimeout)
		adminServer = admin.New(admin.Config{
			Addr:          cfg.MetricsAddr,
			ReadyCacheTTL: cfg.ReadyCacheTTL,
			ProbeTimeout:  cfg.ConnectTimeout,
		}, prober, m, logger)
		if err := adminServer.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "admin listener error: %v\n", err)
			os.Exit(1)
		}
		logger.Info("admin listener started", "addr", cfg.MetricsAddr)
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay listener error: %v\n", err)
		os.Exit(1)
	}

	server := bridge.New(bridge.Config{
		VsockCID:          cfg.VsockCID,
		VsockPort:         cfg.VsockPort,
		ConnectTimeout:    cfg.ConnectTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxConnLifetime:   cfg.MaxConnLifetime,
		MaxConns:          cfg.MaxConns,
		TCPKeepAlive:      cfg.TCPKeepAlive,
		ShutdownTimeout:   cfg.ShutdownTimeout,
		CopyBufferSize:    64 * 1024,
		ShutdownStartedFn: func() { adminServerSetDraining(adminServer) },
	}, dialer, m, logger)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("relay listener started", "addr", cfg.ListenAddr, "vsock_cid", cfg.VsockCID, "vsock_port", cfg.VsockPort)
		errCh <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-errCh:
		if err != nil {
			logger.Error("relay listener stopped", "error", err)
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("relay shutdown incomplete", "error", err)
		os.Exit(1)
	}
	if adminServer != nil {
		adminServer.SetDraining()
		_ = adminServer.Shutdown(context.Background())
	}
	logger.Info("shutdown complete")
}

func adminServerSetDraining(s *admin.Server) {
	if s != nil {
		s.SetDraining()
	}
}

func logLevel(raw string) slog.Level {
	switch strings.ToLower(raw) {
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
