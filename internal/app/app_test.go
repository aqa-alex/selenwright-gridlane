package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gridlane/internal/auth"
	"gridlane/internal/catalog"
	"gridlane/internal/config"
	"gridlane/internal/health"
)

func TestParseFlagsDefaults(t *testing.T) {
	opts, showVersion, err := ParseFlags(nil)
	if err != nil {
		t.Fatalf("ParseFlags() error = %v", err)
	}
	if showVersion {
		t.Fatal("ParseFlags() showVersion = true, want false")
	}
	if opts.Listen != ":4444" {
		t.Fatalf("Listen = %q, want :4444", opts.Listen)
	}
	if opts.ConfigPath != "router.json" {
		t.Fatalf("ConfigPath = %q, want router.json", opts.ConfigPath)
	}
	if opts.GracefulPeriod != 15*time.Second {
		t.Fatalf("GracefulPeriod = %s, want 15s", opts.GracefulPeriod)
	}
	if opts.SessionAttemptTimeout != 30*time.Second {
		t.Fatalf("SessionAttemptTimeout = %s, want 30s", opts.SessionAttemptTimeout)
	}
	if opts.ProxyTimeout != 5*time.Minute {
		t.Fatalf("ProxyTimeout = %s, want 5m", opts.ProxyTimeout)
	}
	if !opts.ReloadOnSIGHUP {
		t.Fatal("ReloadOnSIGHUP = false, want true")
	}
}

func TestParseFlagsRejectsUnknownLogFormat(t *testing.T) {
	_, _, err := ParseFlags([]string{"-log-format", "yaml"})
	if err == nil {
		t.Fatal("ParseFlags() error = nil, want error")
	}
}

func TestNewHandlerPlaceholderEndpoints(t *testing.T) {
	handler := NewHandler(Options{ConfigPath: "/tmp/router.json"}, newTestRuntime(t))

	tests := []struct {
		name       string
		path       string
		wantStatus string
	}{
		{name: "ping", path: "/ping", wantStatus: "ok"},
		{name: "status", path: "/status", wantStatus: "ok"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status code = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}

			var payload map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if payload["service"] != serviceName {
				t.Fatalf("service = %v, want %s", payload["service"], serviceName)
			}
			if payload["status"] != tt.wantStatus {
				t.Fatalf("status = %v, want %s", payload["status"], tt.wantStatus)
			}
		})
	}
}

func TestNewHandlerRejectsNonGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ping", nil)
	rec := httptest.NewRecorder()

	NewHandler(Options{}, newTestRuntime(t)).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want GET, HEAD", got)
	}
}

func TestNewHandlerConfigRequiresAdminToken(t *testing.T) {
	runtime := newTestRuntime(t)
	handler := NewHandler(Options{ConfigPath: "/tmp/router.json"}, runtime)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status code without token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodGet, "/config", nil)
	req.Header.Set(auth.HeaderAdminToken, "root-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code with token = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode config response: %v", err)
	}
	if payload["service"] != serviceName {
		t.Fatalf("service = %v, want %s", payload["service"], serviceName)
	}
	admin, ok := payload["admin"].(map[string]any)
	if !ok {
		t.Fatalf("admin = %T, want object", payload["admin"])
	}
	if admin["token_configured"] != true {
		t.Fatalf("admin.token_configured = %v, want true", admin["token_configured"])
	}
	if strings.Contains(body, "root-token") || strings.Contains(body, "wonderland") {
		t.Fatalf("config response leaked secret: %s", body)
	}
}

func TestNewHandlerMetricsRequiresAdminToken(t *testing.T) {
	handler := NewHandler(Options{}, newTestRuntime(t))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status code without token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set(auth.HeaderAdminToken, "root-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code with token = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `gridlane_http_requests_total{method="GET",route="/metrics",status="401"} 1`) {
		t.Fatalf("metrics did not include previous unauthorized scrape:\n%s", body)
	}
}

func TestNewHandlerQuotaAllowsBasicAuth(t *testing.T) {
	handler := NewHandler(Options{}, newTestRuntime(t))
	req := httptest.NewRequest(http.MethodGet, "/quota", nil)
	req.SetBasicAuth("alice", "wonderland")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode quota response: %v", err)
	}
	if payload["service"] != serviceName {
		t.Fatalf("service = %v, want %s", payload["service"], serviceName)
	}
}

func TestNewHandlerSideEndpointsRequireBasicAuthEvenWithGuest(t *testing.T) {
	handler := NewHandler(Options{}, newTestRuntime(t))

	for _, path := range []string{
		"/vnc/session-id",
		"/devtools/session-id/page",
		"/video/session-id",
		"/logs/session-id",
		"/download/session-id/report.txt",
		"/downloads/session-id/report.txt",
		"/clipboard/session-id",
		"/history/settings",
		"/host/session-id",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status code = %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got == "" {
			t.Fatalf("%s WWW-Authenticate header is empty", path)
		}
	}
}

func TestNewHandlerPlaywrightUsesUserScope(t *testing.T) {
	handler := NewHandler(Options{}, newTestRuntime(t))
	req := httptest.NewRequest(http.MethodGet, "/playwright/chrome/stable", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func newTestRuntime(t *testing.T) Runtime {
	t.Helper()
	t.Setenv("ALICE_PASSWORD", "wonderland")
	t.Setenv("GRIDLANE_ADMIN_TOKEN", "root-token")

	cfg := config.Config{
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
	cat, err := catalog.New(cfg)
	if err != nil {
		t.Fatalf("catalog.New() error = %v", err)
	}
	policy, err := auth.NewPolicy(cfg, auth.EnvFileResolver{})
	if err != nil {
		t.Fatalf("auth.NewPolicy() error = %v", err)
	}
	return Runtime{Catalog: cat, Auth: policy, Health: health.NewManager(cfg.BackendPools)}
}
