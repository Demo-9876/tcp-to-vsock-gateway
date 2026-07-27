package config

import (
	"testing"
	"time"
)

func TestLoadFromEnvValidatesRequiredValues(t *testing.T) {
	t.Setenv("TTVG_LISTEN_ADDR", "127.0.0.1:15005")
	t.Setenv("TTVG_VSOCK_CID", "4")
	t.Setenv("TTVG_READY_CACHE_TTL", "2s")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.VsockCID != 4 {
		t.Fatalf("VsockCID = %d, want 4", cfg.VsockCID)
	}
	if cfg.ReadyCacheTTL != 2*time.Second {
		t.Fatalf("ReadyCacheTTL = %s, want 2s", cfg.ReadyCacheTTL)
	}
	if cfg.MetricsAddr != DefaultMetricsAddr {
		t.Fatalf("MetricsAddr = %q, want default", cfg.MetricsAddr)
	}
	if cfg.AdminListen != DefaultMetricsAddr {
		t.Fatalf("AdminListen = %q, want legacy default metrics addr", cfg.AdminListen)
	}
}

func TestLoadFromEnvRejectsMissingListenAddr(t *testing.T) {
	t.Setenv("TTVG_VSOCK_CID", "4")
	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("LoadFromEnv() expected error")
	}
}

func TestLoadFromEnvUsesParentAdminDefaultWhenProofRelayEnabled(t *testing.T) {
	t.Setenv("POO_PARENT_PROOF_RELAY_LISTEN", "127.0.0.1:15005")
	t.Setenv("POO_PARENT_VSOCK_CID", "4")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() error = %v", err)
	}
	if cfg.AdminListen != DefaultAdminListen {
		t.Fatalf("AdminListen = %q, want %q", cfg.AdminListen, DefaultAdminListen)
	}
}

func TestValidateRejectsInvalidLogLevel(t *testing.T) {
	cfg := Config{
		ListenAddr:       "127.0.0.1:15005",
		VsockCID:         4,
		VsockPort:        5005,
		VsockMetricsPort: 5006,
		ConnectTimeout:   time.Second,
		MaxConns:         1,
		ShutdownTimeout:  time.Second,
		LogLevel:         "trace",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error")
	}
}

func TestValidateRejectsMTLSWithoutFiles(t *testing.T) {
	cfg := Config{
		ProofRelayListen:              "10.0.0.5:15005",
		AuthMode:                      "mtls",
		VsockCID:                      4,
		VsockPort:                     5005,
		VsockMetricsPort:              5006,
		ConnectTimeout:                time.Second,
		MaxConns:                      1,
		ShutdownTimeout:               time.Second,
		LogLevel:                      "info",
		EgressPortStart:               18000,
		EgressPortEnd:                 18001,
		EgressRouteIdleTTL:            time.Second,
		EgressLeaseTTL:                time.Second,
		EgressConnectTimeout:          time.Second,
		EgressMaxActiveRoutes:         1,
		EgressMaxActiveLeases:         1,
		EgressDefaultRouteConcurrency: 1,
		EgressMaxRouteConcurrency:     1,
		EgressLaneListenMode:          "direct-vsock",
		EgressAllowedTargets:          []string{"api.openai.com:443"},
		RelayMaxMetadataBytes:         1,
		RelayMaxReqHeadBytes:          1,
		RelayMaxFrameBytes:            1,
		RelayMaxRequestBytes:          2,
		RelayMaxBufferedBytes:         1,
		RelaySpoolDir:                 t.TempDir(),
		RelayMaxSpoolBytes:            1,
		RelayIOTimeout:                time.Second,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error")
	}
}
