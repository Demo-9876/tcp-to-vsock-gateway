package clientpolicy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLSubset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	err := os.WriteFile(path, []byte(`
clients:
  - san_uri: "spiffe://example/prod/sub2api"
    allowed_targets: ["api.openai.com:443", "api.anthropic.com:443"]
    max_concurrency: 4
`), 0o600)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	set, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	p, ok := set.Lookup("spiffe://example/prod/sub2api")
	if !ok {
		t.Fatal("policy not found")
	}
	if p.MaxConcurrency != 4 {
		t.Fatalf("MaxConcurrency = %d, want 4", p.MaxConcurrency)
	}
	if len(p.AllowedTargets) != 2 || p.AllowedTargets[0] != "api.openai.com:443" {
		t.Fatalf("AllowedTargets = %#v", p.AllowedTargets)
	}
}

func TestLoadYAMLSubsetWithMultilineAllowedTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	err := os.WriteFile(path, []byte(`
clients:
  - san_dns: sub2api.internal
    allowed_targets:
      - api.openai.com:443
      - api.anthropic.com:443
    max_concurrency: 8
`), 0o600)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	set, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	p, ok := set.Lookup("sub2api.internal")
	if !ok {
		t.Fatal("policy not found")
	}
	if p.MaxConcurrency != 8 {
		t.Fatalf("MaxConcurrency = %d, want 8", p.MaxConcurrency)
	}
	if len(p.AllowedTargets) != 2 || p.AllowedTargets[1] != "api.anthropic.com:443" {
		t.Fatalf("AllowedTargets = %#v", p.AllowedTargets)
	}
}

func TestRejectDuplicateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	err := os.WriteFile(path, []byte(`{"clients":[{"san_uri":"id"},{"san_uri":"id"}]}`), 0o600)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() expected duplicate identity error")
	}
}

func TestRejectMultipleIdentitiesInOnePolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	err := os.WriteFile(path, []byte(`{"clients":[{"san_uri":"id","subject":"CN=x"}]}`), 0o600)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() expected multiple identities error")
	}
}

func TestSubjectPolicyIsCanonicalized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	err := os.WriteFile(path, []byte(`{"clients":[{"subject":"O=example, CN=sub2api-prod"}]}`), 0o600)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	set, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, ok := set.Lookup("CN=sub2api-prod,O=example"); !ok {
		t.Fatal("canonical subject policy not found")
	}
}

func TestRejectInvalidAllowedTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	err := os.WriteFile(path, []byte(`{"clients":[{"san_uri":"id","allowed_targets":["api.openai.com"]}]}`), 0o600)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() expected invalid allowed target error")
	}
}

func TestRejectEmptyPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	err := os.WriteFile(path, []byte(`{"clients":[]}`), 0o600)
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() expected empty policy error")
	}
}
