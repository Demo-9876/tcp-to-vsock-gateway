package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/clientpolicy"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/config"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/egressroute"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/metrics"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/proofrelay"
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
	var printDefaultTargets bool
	flag.BoolVar(&printDefaultTargets, "print-default-targets", false, "print built-in default allowed targets and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("tcp-to-vsock-gateway version=%s commit=%s build_time=%s go=%s\n", version, commit, buildTime, config.GoVersion())
		return
	}
	if printDefaultTargets {
		for _, target := range config.DefaultAllowedTargets() {
			fmt.Println(target)
		}
		return
	}

	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(2)
	}
	if checkConfig {
		if err := validateCheckConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
			os.Exit(2)
		}
		fmt.Println("configuration ok")
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	m := metrics.New()
	dialer := vsockdial.New()
	laneFactory := laneListenerFactory(cfg)
	routeManager := egressroute.New(egressroute.Config{
		PortStart:               cfg.EgressPortStart,
		PortEnd:                 cfg.EgressPortEnd,
		PortCooldown:            cfg.EgressPortCooldown,
		RouteIdleTTL:            cfg.EgressRouteIdleTTL,
		LeaseTTL:                cfg.EgressLeaseTTL,
		ConnectTimeout:          cfg.EgressConnectTimeout,
		IdleTimeout:             cfg.IdleTimeout,
		MaxActiveRoutes:         cfg.EgressMaxActiveRoutes,
		MaxActiveLeases:         cfg.EgressMaxActiveLeases,
		DefaultRouteConcurrency: cfg.EgressDefaultRouteConcurrency,
		MaxRouteConcurrency:     cfg.EgressMaxRouteConcurrency,
		AllowedTargets:          cfg.EgressAllowedTargets,
		CopyBufferSize:          64 * 1024,
		Metrics:                 m,
	}, laneFactory, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var adminServer *admin.Server
	if cfg.AdminListen != "" {
		prober := admin.NewVsockMetricsProber(dialer, cfg.VsockCID, cfg.VsockMetricsPort, cfg.ConnectTimeout)
		adminServer = admin.New(admin.Config{
			Addr:          cfg.AdminListen,
			ReadyCacheTTL: cfg.ReadyCacheTTL,
			ProbeTimeout:  cfg.ConnectTimeout,
		}, prober, m, logger)
		if err := adminServer.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "admin listener error: %v\n", err)
			os.Exit(1)
		}
		logger.Info("admin listener started", "addr", cfg.AdminListen)
	}

	var legacyServer *bridge.Server
	var proofServer *proofrelay.Server
	errCh := make(chan error, 2)
	if cfg.LegacyIngressListen != "" {
		listener, err := net.Listen("tcp", cfg.LegacyIngressListen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "legacy relay listener error: %v\n", err)
			os.Exit(1)
		}
		legacyServer = bridge.New(bridge.Config{
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
		go func() {
			logger.Info("legacy relay listener started", "addr", cfg.LegacyIngressListen, "vsock_cid", cfg.VsockCID, "vsock_port", cfg.VsockPort)
			errCh <- legacyServer.Serve(listener)
		}()
	}
	if cfg.ProofRelayListen != "" {
		tlsConfig, err := proofRelayTLSConfig(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "proof relay TLS configuration error: %v\n", err)
			os.Exit(2)
		}
		policies, err := clientpolicy.Load(cfg.ClientPolicyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "client policy error: %v\n", err)
			os.Exit(2)
		}
		if err := validateEffectiveTargetAllowlist(cfg, policies); err != nil {
			fmt.Fprintf(os.Stderr, "client policy error: %v\n", err)
			os.Exit(2)
		}
		proofServer = proofrelay.New(proofrelay.Config{
			Addr:              cfg.ProofRelayListen,
			VsockCID:          cfg.VsockCID,
			VsockPort:         cfg.VsockPort,
			MaxMetadataBytes:  cfg.RelayMaxMetadataBytes,
			MaxReqHeadBytes:   cfg.RelayMaxReqHeadBytes,
			MaxFrameBytes:     cfg.RelayMaxFrameBytes,
			MaxRequestBytes:   cfg.RelayMaxRequestBytes,
			MaxBufferedBytes:  cfg.RelayMaxBufferedBytes,
			SpoolDir:          cfg.RelaySpoolDir,
			MaxSpoolBytes:     cfg.RelayMaxSpoolBytes,
			IOTimeout:         cfg.RelayIOTimeout,
			ReadHeaderTimeout: cfg.ConnectTimeout,
			ShutdownTimeout:   cfg.ShutdownTimeout,
			TLSConfig:         tlsConfig,
			ClientPolicies:    policies,
			Metrics:           m,
		}, dialer, routeManager, logger)
		if err := proofServer.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "proof relay listener error: %v\n", err)
			os.Exit(1)
		}
		go func() {
			if err, ok := <-proofServer.ErrorC(); ok && err != nil {
				errCh <- err
			}
		}()
		logger.Info("proof relay listener started", "addr", cfg.ProofRelayListen, "vsock_cid", cfg.VsockCID, "vsock_port", cfg.VsockPort)
	}

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
	routeManager.Shutdown()
	if proofServer != nil {
		if err := proofServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("proof relay shutdown incomplete", "error", err)
			os.Exit(1)
		}
	}
	if legacyServer != nil {
		if err := legacyServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("legacy relay shutdown incomplete", "error", err)
			os.Exit(1)
		}
	}
	if adminServer != nil {
		adminServer.SetDraining()
		_ = adminServer.Shutdown(context.Background())
	}
	logger.Info("shutdown complete")
}

func laneListenerFactory(cfg config.Config) egressroute.ListenerFactory {
	if cfg.EgressLaneListenMode == "tcp-loopback" {
		return egressroute.TCPListenerFactory{Host: "127.0.0.1"}
	}
	return vsockdial.NewListenerFactory()
}

func proofRelayTLSConfig(cfg config.Config) (*tls.Config, error) {
	if cfg.AuthMode != "mtls" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.MTLSCertFile, cfg.MTLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.MTLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("client CA file did not contain a PEM certificate")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

func validateCheckConfig(cfg config.Config) error {
	if cfg.ProofRelayListen == "" {
		return nil
	}
	if cfg.AuthMode == "mtls" {
		if _, err := proofRelayTLSConfig(cfg); err != nil {
			return fmt.Errorf("proof relay TLS configuration: %w", err)
		}
	}
	if cfg.ClientPolicyFile != "" {
		policies, err := clientpolicy.Load(cfg.ClientPolicyFile)
		if err != nil {
			return fmt.Errorf("client policy: %w", err)
		}
		if err := validateEffectiveTargetAllowlist(cfg, policies); err != nil {
			return err
		}
	}
	return nil
}

func validateEffectiveTargetAllowlist(cfg config.Config, policies *clientpolicy.PolicySet) error {
	if policies == nil {
		return nil
	}
	for _, policy := range policies.Clients {
		if len(policy.AllowedTargets) == 0 {
			continue
		}
		if !hasTargetIntersection(cfg.EgressAllowedTargets, policy.AllowedTargets) {
			return fmt.Errorf("client policy %q effective target allowlist is empty", policy.Identity())
		}
	}
	return nil
}

func hasTargetIntersection(global, client []string) bool {
	globalSet := make(map[string]struct{}, len(global))
	for _, target := range global {
		globalSet[strings.ToLower(strings.TrimSpace(target))] = struct{}{}
	}
	for _, target := range client {
		if _, ok := globalSet[strings.ToLower(strings.TrimSpace(target))]; ok {
			return true
		}
	}
	return false
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
