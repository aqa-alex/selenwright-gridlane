package auth

import (
	"net/http/httptest"
	"testing"

	"gridlane/internal/config"
)

func TestPolicyAuthenticatesUserGuestAndAdmin(t *testing.T) {
	t.Setenv("ALICE_PASSWORD", "wonderland")
	t.Setenv("GRIDLANE_ADMIN_TOKEN", "root-token")

	policy, err := NewPolicy(sampleConfig(), EnvFileResolver{})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	if !policy.AuthenticateBasic("alice", "wonderland") {
		t.Fatal("AuthenticateBasic(alice) = false, want true")
	}
	if policy.AuthenticateBasic("alice", "wrong") {
		t.Fatal("AuthenticateBasic(wrong) = true, want false")
	}
	if !policy.GuestEnabled() {
		t.Fatal("GuestEnabled() = false, want true")
	}
	if !policy.AuthenticateAdminToken("root-token") {
		t.Fatal("AuthenticateAdminToken() = false, want true")
	}
}

func TestAuthorizeUserScopeFallsBackToGuest(t *testing.T) {
	t.Setenv("ALICE_PASSWORD", "wonderland")

	cfg := sampleConfig()
	cfg.Admin.TokenRef = ""
	policy, err := NewPolicy(cfg, EnvFileResolver{})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}

	identity := policy.Authorize(httptest.NewRequest("GET", "/quota", nil), ScopeUser)
	if !identity.Allowed {
		t.Fatal("Authorize(user) = false, want guest fallback")
	}
	if !identity.Guest {
		t.Fatal("Guest = false, want true")
	}
}

func TestAuthorizeAdminScopeRequiresAdminToken(t *testing.T) {
	t.Setenv("ALICE_PASSWORD", "wonderland")
	t.Setenv("GRIDLANE_ADMIN_TOKEN", "root-token")

	policy, err := NewPolicy(sampleConfig(), EnvFileResolver{})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/config", nil)
	if policy.Authorize(req, ScopeAdmin).Allowed {
		t.Fatal("Authorize(admin without token) = true, want false")
	}

	req.Header.Set(HeaderAdminToken, "root-token")
	identity := policy.Authorize(req, ScopeAdmin)
	if !identity.Allowed {
		t.Fatal("Authorize(admin with token) = false, want true")
	}
	if !identity.Admin {
		t.Fatal("Admin = false, want true")
	}
}

func TestAuthorizeSideScopeDoesNotFallBackToGuest(t *testing.T) {
	t.Setenv("ALICE_PASSWORD", "wonderland")

	cfg := sampleConfig()
	cfg.Admin.TokenRef = ""
	policy, err := NewPolicy(cfg, EnvFileResolver{})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}

	req := httptest.NewRequest("GET", "/vnc/session-id", nil)
	if policy.Authorize(req, ScopeSide).Allowed {
		t.Fatal("Authorize(side without basic auth) = true, want false")
	}

	req.SetBasicAuth("alice", "wonderland")
	if !policy.Authorize(req, ScopeSide).Allowed {
		t.Fatal("Authorize(side with basic auth) = false, want true")
	}
}

func TestScopeForPath(t *testing.T) {
	tests := map[string]Scope{
		"/ping":                     ScopePublic,
		"/status":                   ScopePublic,
		"/config":                   ScopeAdmin,
		"/metrics":                  ScopeAdmin,
		"/quota":                    ScopeUser,
		"/host/session-id":          ScopeSide,
		"/vnc/session-id":           ScopeSide,
		"/devtools/session-id/page": ScopeSide,
		"/video/session-id":         ScopeSide,
		"/logs/session-id":          ScopeSide,
		"/download/session-id/name": ScopeSide,
		"/downloads/session-id":     ScopeSide,
		"/clipboard/session-id":     ScopeSide,
		"/history/settings":         ScopeSide,
	}

	for path, want := range tests {
		if got := ScopeForPath(path); got != want {
			t.Fatalf("ScopeForPath(%q) = %s, want %s", path, got, want)
		}
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
			Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
		}}},
		BackendPools: []config.BackendPool{{
			ID:        "sw-local",
			Endpoint:  "http://127.0.0.1:4444",
			Region:    "local",
			Weight:    1,
			Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
		}},
		Admin: config.Admin{TokenRef: "env:GRIDLANE_ADMIN_TOKEN"},
	}
}
