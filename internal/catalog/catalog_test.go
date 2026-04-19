package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"gridlane/internal/config"
)

func TestSanitizedConfigRedactsSecretRefs(t *testing.T) {
	t.Parallel()
	cat, err := New(sampleConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	view := cat.SanitizedConfig("/etc/gridlane/router.json")
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	serialized := string(payload)

	for _, forbidden := range []string{
		"env:ALICE_PASSWORD",
		"env:GRIDLANE_ADMIN_TOKEN",
		"env:BACKEND_USER",
		"file:/run/secrets/backend_password",
	} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("sanitized config leaked %q in %s", forbidden, serialized)
		}
	}
	if !view.Users[0].PasswordConfigured {
		t.Fatal("PasswordConfigured = false, want true")
	}
	if !view.BackendPools[0].CredentialsConfigured {
		t.Fatal("CredentialsConfigured = false, want true")
	}
	if !view.Admin.TokenConfigured {
		t.Fatal("TokenConfigured = false, want true")
	}
}

func TestQuotaViewIncludesUsersAndGuest(t *testing.T) {
	t.Parallel()
	cat, err := New(sampleConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	view := cat.Quota()
	if len(view.Users) != 1 {
		t.Fatalf("users = %d, want 1", len(view.Users))
	}
	if view.Users[0].Name != "alice" {
		t.Fatalf("user name = %q, want alice", view.Users[0].Name)
	}
	if view.Users[0].Quota.MaxSessions != 2 {
		t.Fatalf("user max sessions = %d, want 2", view.Users[0].Quota.MaxSessions)
	}
	if view.Guest == nil {
		t.Fatal("guest = nil, want configured guest quota")
	}
	if view.Guest.Quota.MaxSessions != 1 {
		t.Fatalf("guest max sessions = %d, want 1", view.Guest.Quota.MaxSessions)
	}
}

func sampleConfig() config.Config {
	return config.Config{
		Version: config.Version,
		Users: []config.User{{
			Name:        "alice",
			PasswordRef: "env:ALICE_PASSWORD",
			Quota:       config.Quota{MaxSessions: 2},
		}},
		Guest: &config.Guest{Quota: config.Quota{MaxSessions: 1}},
		Catalog: config.Catalog{Browsers: []config.Browser{{
			Name:      "chrome",
			Versions:  []string{"stable"},
			Platforms: []string{"linux"},
			Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
		}}},
		BackendPools: []config.BackendPool{{
			ID:        "sw-local",
			Endpoint:  "http://127.0.0.1:4444",
			Region:    "local",
			Weight:    1,
			Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
			Credentials: &config.BackendCredentials{
				UsernameRef: "env:BACKEND_USER",
				PasswordRef: "file:/run/secrets/backend_password",
			},
			Health: config.HealthPolicy{Enabled: true, FailureThreshold: 3, Cooldown: "30s"},
		}},
		Admin: config.Admin{TokenRef: "env:GRIDLANE_ADMIN_TOKEN"},
	}
}
