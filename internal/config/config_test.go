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
}

func TestLoadFromEnvRejectsMissingListenAddr(t *testing.T) {
	t.Setenv("TTVG_VSOCK_CID", "4")
	_, err := LoadFromEnv()
	if err == nil {
		t.Fatal("LoadFromEnv() expected error")
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
