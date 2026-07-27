package config

import (
	"fmt"
	"net"
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
	DefaultProofRelayListen               = ""
	DefaultAdminListen                    = "127.0.0.1:15007"
	DefaultAuthMode                       = "none"
	DefaultConnectTimeout                 = 5 * time.Second
	DefaultIdleTimeout                    = 300 * time.Second
	DefaultReadyCacheTTL                  = time.Second
	DefaultMaxConnLifetime  time.Duration = 0
	DefaultMaxConns                       = 1024
	DefaultTCPKeepAlive                   = 30 * time.Second
	DefaultShutdownTimeout                = 300 * time.Second
	DefaultLogLevel                       = "info"

	DefaultEgressPortRange                     = "18000-18999"
	DefaultEgressPortCooldown                  = 5 * time.Second
	DefaultEgressRouteIdleTTL                  = 5 * time.Minute
	DefaultEgressLeaseTTL                      = 30 * time.Second
	DefaultEgressConnectTimeout                = 10 * time.Second
	DefaultEgressMaxActiveRoutes               = 1000
	DefaultEgressMaxActiveLeases               = 4096
	DefaultEgressDefaultRouteConcurrency       = 1
	DefaultEgressMaxRouteConcurrency           = 16
	DefaultEgressLaneListenMode                = "direct-vsock"
	DefaultRelayMaxMetadataBytes         int64 = 16 * 1024
	DefaultRelayMaxReqHeadBytes          int64 = 1024 * 1024
	DefaultRelayMaxFrameBytes            int64 = 64 * 1024 * 1024
	DefaultRelayMaxRequestBytes          int64 = 256 * 1024 * 1024
	DefaultRelayMaxBufferedBytes         int64 = 4 * 1024 * 1024
	DefaultRelayMaxSpoolBytes            int64 = 1024 * 1024 * 1024
	DefaultRelayIOTimeout                      = 300 * time.Second
)

type Config struct {
	ListenAddr          string // Legacy alias for LegacyIngressListen.
	ProofRelayListen    string
	LegacyIngressListen string
	AdminListen         string
	AuthMode            string
	MTLSCAFile          string
	MTLSCertFile        string
	MTLSKeyFile         string
	ClientPolicyFile    string
	VsockCID            uint32
	VsockPort           uint32
	MetricsAddr         string
	VsockMetricsPort    uint32
	ReadyCacheTTL       time.Duration
	ConnectTimeout      time.Duration
	IdleTimeout         time.Duration
	MaxConnLifetime     time.Duration
	MaxConns            int
	TCPKeepAlive        time.Duration
	ShutdownTimeout     time.Duration
	LogLevel            string

	EgressPortRange               string
	EgressPortStart               uint32
	EgressPortEnd                 uint32
	EgressPortCooldown            time.Duration
	EgressRouteIdleTTL            time.Duration
	EgressLeaseTTL                time.Duration
	EgressConnectTimeout          time.Duration
	EgressMaxActiveRoutes         int
	EgressMaxActiveLeases         int
	EgressDefaultRouteConcurrency int
	EgressMaxRouteConcurrency     int
	EgressLaneListenMode          string
	EgressAllowedTargets          []string

	RelayMaxMetadataBytes int64
	RelayMaxReqHeadBytes  int64
	RelayMaxFrameBytes    int64
	RelayMaxRequestBytes  int64
	RelayMaxBufferedBytes int64
	RelaySpoolDir         string
	RelayMaxSpoolBytes    int64
	RelayIOTimeout        time.Duration
}

func LoadFromEnv() (Config, error) {
	cfg := Config{
		ListenAddr:       strings.TrimSpace(os.Getenv("TTVG_LISTEN_ADDR")),
		ProofRelayListen: getenvTrimDefault("POO_PARENT_PROOF_RELAY_LISTEN", DefaultProofRelayListen),
		AuthMode:         DefaultAuthMode,
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

		EgressPortRange:               DefaultEgressPortRange,
		EgressPortCooldown:            DefaultEgressPortCooldown,
		EgressRouteIdleTTL:            DefaultEgressRouteIdleTTL,
		EgressLeaseTTL:                DefaultEgressLeaseTTL,
		EgressConnectTimeout:          DefaultEgressConnectTimeout,
		EgressMaxActiveRoutes:         DefaultEgressMaxActiveRoutes,
		EgressMaxActiveLeases:         DefaultEgressMaxActiveLeases,
		EgressDefaultRouteConcurrency: DefaultEgressDefaultRouteConcurrency,
		EgressMaxRouteConcurrency:     DefaultEgressMaxRouteConcurrency,
		EgressLaneListenMode:          DefaultEgressLaneListenMode,
		EgressAllowedTargets:          DefaultAllowedTargets(),

		RelayMaxMetadataBytes: DefaultRelayMaxMetadataBytes,
		RelayMaxReqHeadBytes:  DefaultRelayMaxReqHeadBytes,
		RelayMaxFrameBytes:    DefaultRelayMaxFrameBytes,
		RelayMaxRequestBytes:  DefaultRelayMaxRequestBytes,
		RelayMaxBufferedBytes: DefaultRelayMaxBufferedBytes,
		RelaySpoolDir:         os.TempDir(),
		RelayMaxSpoolBytes:    DefaultRelayMaxSpoolBytes,
		RelayIOTimeout:        DefaultRelayIOTimeout,
	}

	var err error
	if cfg.LegacyIngressListen = getenvTrimDefault("POO_PARENT_LEGACY_INGRESS_LISTEN", cfg.ListenAddr); cfg.LegacyIngressListen == "" {
		cfg.LegacyIngressListen = cfg.ListenAddr
	}
	if cfg.VsockCID, err = envUint32AnyRequired("POO_PARENT_VSOCK_CID", "TTVG_VSOCK_CID"); err != nil {
		return Config{}, err
	}
	if cfg.VsockPort, err = envUint32AnyDefault(cfg.VsockPort, "POO_PARENT_VSOCK_PORT", "TTVG_VSOCK_PORT"); err != nil {
		return Config{}, err
	}
	if v := strings.TrimSpace(os.Getenv("POO_PARENT_AUTH_MODE")); v != "" {
		cfg.AuthMode = strings.ToLower(v)
	}
	cfg.MTLSCAFile = strings.TrimSpace(os.Getenv("POO_PARENT_MTLS_CA_FILE"))
	cfg.MTLSCertFile = strings.TrimSpace(os.Getenv("POO_PARENT_MTLS_CERT_FILE"))
	cfg.MTLSKeyFile = strings.TrimSpace(os.Getenv("POO_PARENT_MTLS_KEY_FILE"))
	cfg.ClientPolicyFile = strings.TrimSpace(os.Getenv("POO_PARENT_CLIENT_POLICY_FILE"))
	if v, ok := os.LookupEnv("TTVG_METRICS_ADDR"); ok {
		cfg.MetricsAddr = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("POO_PARENT_ADMIN_LISTEN"); ok {
		cfg.AdminListen = strings.TrimSpace(v)
		cfg.MetricsAddr = cfg.AdminListen
	} else if cfg.ProofRelayListen != "" {
		cfg.AdminListen = DefaultAdminListen
		cfg.MetricsAddr = cfg.AdminListen
	} else {
		cfg.AdminListen = cfg.MetricsAddr
	}
	if cfg.VsockMetricsPort, err = envUint32AnyDefault(cfg.VsockMetricsPort, "POO_PARENT_VSOCK_METRICS_PORT", "TTVG_VSOCK_METRICS_PORT"); err != nil {
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
	if v := strings.TrimSpace(os.Getenv("POO_PARENT_LOG_LEVEL")); v != "" {
		cfg.LogLevel = strings.ToLower(v)
	}
	if cfg.ShutdownTimeout, err = envDurationMSDefault("POO_PARENT_SHUTDOWN_TIMEOUT_MS", cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if v := strings.TrimSpace(os.Getenv("POO_EGRESS_PORT_RANGE")); v != "" {
		cfg.EgressPortRange = v
	}
	if cfg.EgressPortStart, cfg.EgressPortEnd, err = parsePortRange("POO_EGRESS_PORT_RANGE", cfg.EgressPortRange); err != nil {
		return Config{}, err
	}
	if cfg.EgressPortCooldown, err = envDurationMSDefault("POO_EGRESS_PORT_COOLDOWN_MS", cfg.EgressPortCooldown); err != nil {
		return Config{}, err
	}
	if cfg.EgressRouteIdleTTL, err = envDurationMSDefault("POO_EGRESS_ROUTE_IDLE_TTL_MS", cfg.EgressRouteIdleTTL); err != nil {
		return Config{}, err
	}
	if cfg.EgressLeaseTTL, err = envDurationMSDefault("POO_EGRESS_LEASE_TTL_MS", cfg.EgressLeaseTTL); err != nil {
		return Config{}, err
	}
	if cfg.EgressConnectTimeout, err = envDurationMSDefault("POO_EGRESS_DEFAULT_CONNECT_TIMEOUT_MS", cfg.EgressConnectTimeout); err != nil {
		return Config{}, err
	}
	if cfg.EgressMaxActiveRoutes, err = envIntDefault("POO_EGRESS_MAX_ACTIVE_ROUTES", cfg.EgressMaxActiveRoutes); err != nil {
		return Config{}, err
	}
	if cfg.EgressMaxActiveLeases, err = envIntDefault("POO_EGRESS_MAX_ACTIVE_LEASES", cfg.EgressMaxActiveLeases); err != nil {
		return Config{}, err
	}
	if cfg.EgressDefaultRouteConcurrency, err = envIntDefault("POO_EGRESS_DEFAULT_ROUTE_CONCURRENCY", cfg.EgressDefaultRouteConcurrency); err != nil {
		return Config{}, err
	}
	if cfg.EgressMaxRouteConcurrency, err = envIntDefault("POO_EGRESS_MAX_ROUTE_CONCURRENCY", cfg.EgressMaxRouteConcurrency); err != nil {
		return Config{}, err
	}
	if v := strings.TrimSpace(os.Getenv("POO_EGRESS_LANE_LISTEN_MODE")); v != "" {
		cfg.EgressLaneListenMode = strings.ToLower(v)
	}
	if v, ok := os.LookupEnv("POO_EGRESS_ALLOWED_TARGETS"); ok {
		cfg.EgressAllowedTargets = parseCSV(v)
		if len(cfg.EgressAllowedTargets) == 0 {
			cfg.EgressAllowedTargets = DefaultAllowedTargets()
		}
	}
	if cfg.RelayMaxMetadataBytes, err = envInt64Default("POO_RELAY_MAX_METADATA_BYTES", cfg.RelayMaxMetadataBytes); err != nil {
		return Config{}, err
	}
	if cfg.RelayMaxReqHeadBytes, err = envInt64Default("POO_RELAY_MAX_REQ_HEAD_BYTES", cfg.RelayMaxReqHeadBytes); err != nil {
		return Config{}, err
	}
	if cfg.RelayMaxFrameBytes, err = envInt64Default("POO_RELAY_MAX_FRAME_BYTES", cfg.RelayMaxFrameBytes); err != nil {
		return Config{}, err
	}
	if cfg.RelayMaxRequestBytes, err = envInt64Default("POO_RELAY_MAX_REQUEST_BYTES", cfg.RelayMaxRequestBytes); err != nil {
		return Config{}, err
	}
	if cfg.RelayMaxBufferedBytes, err = envInt64Default("POO_RELAY_MAX_BUFFERED_BYTES", cfg.RelayMaxBufferedBytes); err != nil {
		return Config{}, err
	}
	if v := strings.TrimSpace(os.Getenv("POO_RELAY_SPOOL_DIR")); v != "" {
		cfg.RelaySpoolDir = v
	}
	if cfg.RelayMaxSpoolBytes, err = envInt64Default("POO_RELAY_MAX_SPOOL_BYTES", cfg.RelayMaxSpoolBytes); err != nil {
		return Config{}, err
	}
	if cfg.RelayIOTimeout, err = envDurationMSDefault("POO_RELAY_IO_TIMEOUT_MS", cfg.RelayIOTimeout); err != nil {
		return Config{}, err
	}

	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if c.ProofRelayListen == "" && c.LegacyIngressListen == "" {
		return fmt.Errorf("POO_PARENT_PROOF_RELAY_LISTEN or POO_PARENT_LEGACY_INGRESS_LISTEN is required")
	}
	if c.VsockCID == 0 {
		return fmt.Errorf("POO_PARENT_VSOCK_CID/TTVG_VSOCK_CID must be greater than 0")
	}
	if c.VsockPort == 0 {
		return fmt.Errorf("POO_PARENT_VSOCK_PORT/TTVG_VSOCK_PORT must be greater than 0")
	}
	if c.VsockMetricsPort == 0 {
		return fmt.Errorf("POO_PARENT_VSOCK_METRICS_PORT/TTVG_VSOCK_METRICS_PORT must be greater than 0")
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
	switch c.AuthMode {
	case "none", "mtls":
	default:
		return fmt.Errorf("POO_PARENT_AUTH_MODE must be none or mtls")
	}
	if c.AuthMode == "none" && c.ProofRelayListen != "" && !isLoopbackListen(c.ProofRelayListen) {
		return fmt.Errorf("POO_PARENT_AUTH_MODE=none requires POO_PARENT_PROOF_RELAY_LISTEN to bind loopback")
	}
	if c.AuthMode == "mtls" {
		if c.ProofRelayListen == "" {
			return fmt.Errorf("POO_PARENT_AUTH_MODE=mtls requires POO_PARENT_PROOF_RELAY_LISTEN")
		}
		if c.MTLSCAFile == "" || c.MTLSCertFile == "" || c.MTLSKeyFile == "" {
			return fmt.Errorf("POO_PARENT_AUTH_MODE=mtls requires POO_PARENT_MTLS_CA_FILE, POO_PARENT_MTLS_CERT_FILE, and POO_PARENT_MTLS_KEY_FILE")
		}
		if c.ClientPolicyFile == "" {
			return fmt.Errorf("POO_PARENT_AUTH_MODE=mtls requires POO_PARENT_CLIENT_POLICY_FILE")
		}
	}
	switch c.EgressLaneListenMode {
	case "direct-vsock", "tcp-loopback":
	default:
		return fmt.Errorf("POO_EGRESS_LANE_LISTEN_MODE must be direct-vsock or tcp-loopback")
	}
	if c.EgressPortStart == 0 || c.EgressPortEnd == 0 || c.EgressPortStart > c.EgressPortEnd {
		return fmt.Errorf("POO_EGRESS_PORT_RANGE is invalid")
	}
	if c.EgressPortCooldown < 0 || c.EgressRouteIdleTTL <= 0 || c.EgressLeaseTTL <= 0 || c.EgressConnectTimeout <= 0 {
		return fmt.Errorf("POO_EGRESS timeout values must be positive except cooldown may be zero")
	}
	if c.EgressMaxActiveRoutes <= 0 || c.EgressMaxActiveLeases <= 0 || c.EgressDefaultRouteConcurrency <= 0 || c.EgressMaxRouteConcurrency <= 0 {
		return fmt.Errorf("POO_EGRESS concurrency and capacity values must be > 0")
	}
	if c.EgressDefaultRouteConcurrency > c.EgressMaxRouteConcurrency {
		return fmt.Errorf("POO_EGRESS_DEFAULT_ROUTE_CONCURRENCY must be <= POO_EGRESS_MAX_ROUTE_CONCURRENCY")
	}
	if len(c.EgressAllowedTargets) == 0 {
		return fmt.Errorf("POO_EGRESS_ALLOWED_TARGETS effective set must not be empty")
	}
	for _, target := range c.EgressAllowedTargets {
		if err := validateTarget(target); err != nil {
			return err
		}
	}
	if c.RelayMaxMetadataBytes <= 0 || c.RelayMaxReqHeadBytes <= 0 || c.RelayMaxFrameBytes <= 0 || c.RelayMaxRequestBytes <= 0 || c.RelayMaxBufferedBytes <= 0 || c.RelayMaxSpoolBytes <= 0 || c.RelayIOTimeout <= 0 {
		return fmt.Errorf("POO_RELAY limits and timeout must be > 0")
	}
	if c.RelayMaxReqHeadBytes > c.RelayMaxFrameBytes {
		return fmt.Errorf("POO_RELAY_MAX_REQ_HEAD_BYTES must be <= POO_RELAY_MAX_FRAME_BYTES")
	}
	if c.RelayMaxReqHeadBytes > 1024*1024 {
		return fmt.Errorf("POO_RELAY_MAX_REQ_HEAD_BYTES must be <= 1048576")
	}
	if c.RelayMaxRequestBytes < c.RelayMaxReqHeadBytes+c.RelayMaxFrameBytes {
		return fmt.Errorf("POO_RELAY_MAX_REQUEST_BYTES must cover REQ_HEAD and REQ_BODY limits")
	}
	if c.RelayMaxBufferedBytes > c.RelayMaxFrameBytes {
		return fmt.Errorf("POO_RELAY_MAX_BUFFERED_BYTES must be <= POO_RELAY_MAX_FRAME_BYTES")
	}
	if c.RelaySpoolDir == "" {
		return fmt.Errorf("POO_RELAY_SPOOL_DIR must not be empty")
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

func envUint32AnyRequired(keys ...string) (uint32, error) {
	for _, key := range keys {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			return parseUint32(key, raw)
		}
	}
	return 0, fmt.Errorf("%s is required", strings.Join(keys, " or "))
}

func envUint32AnyDefault(def uint32, keys ...string) (uint32, error) {
	for _, key := range keys {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			return parseUint32(key, raw)
		}
	}
	return def, nil
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

func envInt64Default(key string, def int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be int64: %w", key, err)
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

func envDurationMSDefault(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be integer milliseconds: %w", key, err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func getenvTrimDefault(key, def string) string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	return raw
}

func parseCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		item := strings.TrimSpace(part)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func parsePortRange(key, raw string) (uint32, uint32, error) {
	parts := strings.Split(raw, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%s must be start-end", key)
	}
	start, err := parseUint32(key, strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	end, err := parseUint32(key, strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	if start == 0 || end == 0 || start > end || end > 65535 {
		return 0, 0, fmt.Errorf("%s must be within 1-65535 and start <= end", key)
	}
	return start, end, nil
}

func isLoopbackListen(addr string) bool {
	host, _, found := strings.Cut(addr, ":")
	if found && host == "" {
		return false
	}
	h, _, splitErr := netSplitHostPort(addr)
	if splitErr == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

func validateTarget(target string) error {
	host, port, err := netSplitHostPort(target)
	if err != nil {
		return fmt.Errorf("POO_EGRESS_ALLOWED_TARGETS item %q must be host:port", target)
	}
	if strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return fmt.Errorf("POO_EGRESS_ALLOWED_TARGETS item %q must be host:port", target)
	}
	if port != "443" {
		return fmt.Errorf("POO_EGRESS_ALLOWED_TARGETS item %q must use port 443 in v1", target)
	}
	return nil
}

var netSplitHostPort = func(s string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(s)
	return host, port, err
}
