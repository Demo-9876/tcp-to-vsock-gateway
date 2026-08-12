package main

import (
	"strings"
	"testing"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/clientpolicy"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/config"
)

func TestValidateEffectiveTargetAllowlistRejectsEmptyIntersection(t *testing.T) {
	err := validateEffectiveTargetAllowlist(config.Config{
		EgressAllowedTargets: []string{"api.openai.com:443"},
	}, &clientpolicy.PolicySet{
		Clients: []clientpolicy.ClientPolicy{{
			SANURI:         "spiffe://example/client",
			AllowedTargets: []string{"api.anthropic.com:443"},
		}},
	})
	if err == nil {
		t.Fatal("validateEffectiveTargetAllowlist() expected error")
	}
	if !strings.Contains(err.Error(), "effective target allowlist is empty") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateEffectiveTargetAllowlistAllowsGlobalFallback(t *testing.T) {
	err := validateEffectiveTargetAllowlist(config.Config{
		EgressAllowedTargets: []string{"api.openai.com:443"},
	}, &clientpolicy.PolicySet{
		Clients: []clientpolicy.ClientPolicy{{
			SANURI: "spiffe://example/client",
		}},
	})
	if err != nil {
		t.Fatalf("validateEffectiveTargetAllowlist() error = %v", err)
	}
}
