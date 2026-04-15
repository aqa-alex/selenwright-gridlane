package config

import (
	"strings"
	"testing"
)

func TestLoadValidConfig(t *testing.T) {
	cfg, err := Load(strings.NewReader(validConfigJSON()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Version != Version {
		t.Fatalf("Version = %d, want %d", cfg.Version, Version)
	}
	if len(cfg.Users) != 1 {
		t.Fatalf("users = %d, want 1", len(cfg.Users))
	}
	if len(cfg.Catalog.Browsers) != 1 {
		t.Fatalf("browsers = %d, want 1", len(cfg.Catalog.Browsers))
	}
	if len(cfg.BackendPools) != 1 {
		t.Fatalf("backend_pools = %d, want 1", len(cfg.BackendPools))
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := Load(strings.NewReader(`{
		"version": 1,
		"unexpected": true,
		"users": [{"name": "alice", "password_ref": "env:ALICE_PASSWORD", "quota": {"max_sessions": 1}}],
		"catalog": {"browsers": [{"name": "chrome", "versions": ["stable"], "protocols": ["webdriver"]}]},
		"backend_pools": [{"id": "local", "endpoint": "http://127.0.0.1:4444", "region": "local", "weight": 1, "protocols": ["webdriver"]}]
	}`))
	if err == nil {
		t.Fatal("Load() error = nil, want unknown field error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %q, want unknown field", err)
	}
}

func TestLoadRejectsMultipleJSONObjects(t *testing.T) {
	_, err := Load(strings.NewReader(validConfigJSON() + `{}`))
	if err == nil {
		t.Fatal("Load() error = nil, want single object error")
	}
	if !strings.Contains(err.Error(), "single JSON object") {
		t.Fatalf("Load() error = %q, want single JSON object", err)
	}
}

func TestValidateRejectsUnsupportedProtocol(t *testing.T) {
	cfg, err := Load(strings.NewReader(validConfigJSON()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.BackendPools[0].Protocols = []Protocol{"cdp"}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want unsupported protocol")
	}
	if !strings.Contains(err.Error(), "unsupported protocol") {
		t.Fatalf("Validate() error = %q, want unsupported protocol", err)
	}
}

func TestValidateRejectsPlaintextSecretRef(t *testing.T) {
	cfg, err := Load(strings.NewReader(validConfigJSON()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.Users[0].PasswordRef = "secret"

	err = cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want secret ref error")
	}
	if !strings.Contains(err.Error(), "env: or file:") {
		t.Fatalf("Validate() error = %q, want secret ref message", err)
	}
}

func validConfigJSON() string {
	return `{
		"version": 1,
		"users": [
			{"name": "alice", "password_ref": "env:ALICE_PASSWORD", "quota": {"max_sessions": 2}}
		],
		"guest": {"quota": {"max_sessions": 1}},
		"catalog": {
			"browsers": [
				{"name": "chrome", "versions": ["stable"], "platforms": ["linux"], "protocols": ["webdriver", "playwright"]}
			]
		},
		"backend_pools": [
			{
				"id": "sw-local",
				"endpoint": "http://127.0.0.1:4444",
				"region": "local",
				"weight": 2,
				"protocols": ["webdriver", "playwright"],
				"credentials": {"username_ref": "env:BACKEND_USER", "password_ref": "file:/run/secrets/backend_password"},
				"health": {"enabled": true, "failure_threshold": 3, "cooldown": "30s"}
			}
		],
		"admin": {"token_ref": "env:GRIDLANE_ADMIN_TOKEN"}
	}`
}
