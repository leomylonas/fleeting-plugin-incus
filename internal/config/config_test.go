package config

import (
	"testing"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"github.com/lxc/incus/v6/shared/api"
)

func TestNormalizePoolKeyDefaultsAndPrefixesUserNamespace(t *testing.T) {
	cfg := validConfig()
	normalized, err := Normalize(cfg, provider.Settings{})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if normalized.PoolConfigKey != DefaultPoolConfigKey {
		t.Fatalf("PoolConfigKey = %q, want %q", normalized.PoolConfigKey, DefaultPoolConfigKey)
	}
	if normalized.NormalizedPoolConfigKey != "user.fleeting.pool" {
		t.Fatalf("NormalizedPoolConfigKey = %q", normalized.NormalizedPoolConfigKey)
	}
}

func TestNormalizeRejectsPoolKeyWithUserPrefix(t *testing.T) {
	cfg := validConfig()
	cfg.PoolConfigKey = "user.fleeting.pool"

	if _, err := Normalize(cfg, provider.Settings{}); err == nil {
		t.Fatal("Normalize() error = nil, want error")
	}
}

func TestNormalizeAllowsPreconfiguredSSHImage(t *testing.T) {
	cfg := validConfig()
	cfg.SSHPublicKey = ""

	normalized, err := Normalize(cfg, provider.Settings{})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if normalized.PublicKey != "" {
		t.Fatalf("PublicKey = %q, want empty", normalized.PublicKey)
	}
}

func TestNormalizeRejectsPrivilegedVM(t *testing.T) {
	cfg := validConfig()
	cfg.InstanceType = InstanceVM
	cfg.Privileged = true

	if _, err := Normalize(cfg, provider.Settings{}); err == nil {
		t.Fatal("Normalize() error = nil, want privileged VM error")
	}
}

func TestNormalizeConnectionValidation(t *testing.T) {
	cfg := validConfig()
	cfg.ConnectionType = ConnectionHTTPS

	if _, err := Normalize(cfg, provider.Settings{}); err == nil {
		t.Fatal("Normalize() https without endpoint error = nil, want error")
	}

	cfg.Endpoint = "https://incus.example.test:8443"
	normalized, err := Normalize(cfg, provider.Settings{})
	if err != nil {
		t.Fatalf("Normalize() https error = %v", err)
	}
	if normalized.TLSClientCert == "" || normalized.TLSClientKey == "" {
		t.Fatal("Normalize() https did not apply default Incus client cert/key paths")
	}
}

func validConfig() Config {
	return Config{
		NamePrefix:   "ci-",
		PoolID:       "test",
		InstanceType: InstanceContainer,
		Image:        api.InstanceSource {
                    Type: "image",
                    Alias: "ubuntu/24.04",
                    Server: "https://images.linuxcontainers.org/",
                    Protocol: "simplestreams",
                },
		SSHUsername:  "runner",
		SSHPublicKey: "ssh-ed25519 AAAATEST test@example",
	}
}
