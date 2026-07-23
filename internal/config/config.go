package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultVsockPort        uint32        = 5005
	DefaultVsockMetricsPort uint32        = 5006
	DefaultMetricsAddr                    = "127.0.0.1:15006"
	DefaultConnectTimeout                 = 5 * time.Second
	DefaultIdleTimeout                    = 300 * time.Second
	DefaultReadyCacheTTL                  = time.Second
	DefaultMaxConnLifetime  time.Duration = 0
	DefaultMaxConns                       = 1024
	DefaultTCPKeepAlive                   = 30 * time.Second
	DefaultShutdownTimeout                = 300 * time.Second
	DefaultLogLevel                       = "info"
)

type Config struct {
	ListenAddr       string
	VsockCID         uint32
	VsockPort        uint32
	MetricsAddr      string
	VsockMetricsPort uint32
	ReadyCacheTTL    time.Duration
	ConnectTimeout   time.Duration
	IdleTimeout      time.Duration
	MaxConnLifetime  time.Duration
	MaxConns         int
	TCPKeepAlive     time.Duration
	ShutdownTimeout  time.Duration
	LogLevel         string
}

func LoadFromEnv() (Config, error) {
	cfg := Config{
		ListenAddr:       strings.TrimSpace(os.Getenv("TTVG_LISTEN_ADDR")),
		VsockPort:        DefaultVsockPort,
		MetricsAddr:      DefaultMetricsAddr,
		VsockMetricsPort: DefaultVsockMetricsPort,
		ReadyCacheTTL:    DefaultReadyCacheTTL,
		ConnectTimeout:   DefaultConnectTimeout,
		IdleTimeout:      DefaultIdleTimeout,
		MaxConnLifetime:  DefaultMaxConnLifetime,
		MaxConns:         DefaultMaxConns,
		TCPKeepAlive:     DefaultTCPKeepAlive,
		ShutdownTimeout:  DefaultShutdownTimeout,
		LogLevel:         DefaultLogLevel,
	}

	var err error
	if cfg.VsockCID, err = envUint32Required("TTVG_VSOCK_CID"); err != nil {
		return Config{}, err
	}
	if cfg.VsockPort, err = envUint32Default("TTVG_VSOCK_PORT", cfg.VsockPort); err != nil {
		return Config{}, err
	}
	if v, ok := os.LookupEnv("TTVG_METRICS_ADDR"); ok {
		cfg.MetricsAddr = strings.TrimSpace(v)
	}
	if cfg.VsockMetricsPort, err = envUint32Default("TTVG_VSOCK_METRICS_PORT", cfg.VsockMetricsPort); err != nil {
		return Config{}, err
	}
	if cfg.ReadyCacheTTL, err = envDurationDefault("TTVG_READY_CACHE_TTL", cfg.ReadyCacheTTL); err != nil {
		return Config{}, err
	}
	if cfg.ConnectTimeout, err = envDurationDefault("TTVG_CONNECT_TIMEOUT", cfg.ConnectTimeout); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = envDurationDefault("TTVG_IDLE_TIMEOUT", cfg.IdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MaxConnLifetime, err = envDurationDefault("TTVG_MAX_CONN_LIFETIME", cfg.MaxConnLifetime); err != nil {
		return Config{}, err
	}
	if cfg.MaxConns, err = envIntDefault("TTVG_MAX_CONNS", cfg.MaxConns); err != nil {
		return Config{}, err
	}
	if cfg.TCPKeepAlive, err = envDurationDefault("TTVG_TCP_KEEPALIVE", cfg.TCPKeepAlive); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = envDurationDefault("TTVG_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if v := strings.TrimSpace(os.Getenv("TTVG_LOG_LEVEL")); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}

	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("TTVG_LISTEN_ADDR is required")
	}
	if c.VsockCID == 0 {
		return fmt.Errorf("TTVG_VSOCK_CID must be greater than 0")
	}
	if c.VsockPort == 0 {
		return fmt.Errorf("TTVG_VSOCK_PORT must be greater than 0")
	}
	if c.VsockMetricsPort == 0 {
		return fmt.Errorf("TTVG_VSOCK_METRICS_PORT must be greater than 0")
	}
	if c.ReadyCacheTTL < 0 {
		return fmt.Errorf("TTVG_READY_CACHE_TTL must be >= 0")
	}
	if c.ConnectTimeout <= 0 {
		return fmt.Errorf("TTVG_CONNECT_TIMEOUT must be > 0")
	}
	if c.IdleTimeout < 0 {
		return fmt.Errorf("TTVG_IDLE_TIMEOUT must be >= 0")
	}
	if c.MaxConnLifetime < 0 {
		return fmt.Errorf("TTVG_MAX_CONN_LIFETIME must be >= 0")
	}
	if c.MaxConns <= 0 {
		return fmt.Errorf("TTVG_MAX_CONNS must be > 0")
	}
	if c.TCPKeepAlive < 0 {
		return fmt.Errorf("TTVG_TCP_KEEPALIVE must be >= 0")
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("TTVG_SHUTDOWN_TIMEOUT must be > 0")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("TTVG_LOG_LEVEL must be debug, info, warn, or error")
	}
	return nil
}

func GoVersion() string {
	return runtime.Version()
}

func envUint32Required(key string) (uint32, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, fmt.Errorf("%s is required", key)
	}
	return parseUint32(key, raw)
}

func envUint32Default(key string, def uint32) (uint32, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	return parseUint32(key, raw)
}

func parseUint32(key, raw string) (uint32, error) {
	v, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be uint32: %w", key, err)
	}
	return uint32(v), nil
}

func envIntDefault(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be int: %w", key, err)
	}
	return v, nil
}

func envDurationDefault(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	if raw == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be duration: %w", key, err)
	}
	return d, nil
}
