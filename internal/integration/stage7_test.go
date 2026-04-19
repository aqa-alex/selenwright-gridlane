package integration

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gridlane/internal/app"
	"gridlane/internal/auth"
	"gridlane/internal/config"
	"gridlane/internal/health"
	"gridlane/internal/sessionid"
)

func TestMixedProtocolIntegrationAcrossTwoFakeSelenwrightBackends(t *testing.T) {
	webdriverBackend := newFakeSelenwright(t, "sw-webdriver")
	defer webdriverBackend.Close()
	playwrightBackend := newFakeSelenwright(t, "sw-playwright")
	defer playwrightBackend.Close()

	cfg := testRouterConfig(
		backendNode{
			ID:        "sw-webdriver",
			Endpoint:  webdriverBackend.URL(),
			Region:    "eu",
			Weight:    2,
			Protocols: []config.Protocol{config.ProtocolWebDriver},
		},
		backendNode{
			ID:        "sw-playwright",
			Endpoint:  playwrightBackend.URL(),
			Region:    "us",
			Weight:    3,
			Protocols: []config.Protocol{config.ProtocolPlaywright},
		},
	)
	router := newRouter(t, cfg)
	defer router.Close()

	webdriverSessionID := createWebDriverSession(t, router.URL, "eu")
	webdriverParts := decodeSessionID(t, webdriverSessionID)
	if webdriverParts.UpstreamSessionID != "sw-webdriver-webdriver-1" {
		t.Fatalf("webdriver upstream session = %q, want sw-webdriver-webdriver-1", webdriverParts.UpstreamSessionID)
	}
	requireRecordedPath(t, webdriverBackend, "POST /wd/hub/session")

	requestOK(t, http.MethodPost, router.URL+"/wd/hub/session/"+webdriverSessionID+"/url", `{"sessionId":"`+webdriverSessionID+`","url":"https://example.test"}`, basicAuth)
	requireRecordedPath(t, webdriverBackend, "POST /wd/hub/session/"+webdriverParts.UpstreamSessionID+"/url")

	playwrightSessionID := openPlaywrightSession(t, router.URL, "/playwright/chrome/stable?trace=1")
	playwrightParts := decodeSessionID(t, playwrightSessionID)
	if !strings.HasPrefix(playwrightParts.UpstreamSessionID, "pw_") {
		t.Fatalf("playwright upstream session = %q, want pw_ prefix", playwrightParts.UpstreamSessionID)
	}
	requireRecordedPath(t, playwrightBackend, "GET /playwright/chrome/stable?trace=1")
	requirePlaywrightExternalID(t, playwrightBackend, playwrightSessionID)

	exerciseWebDriverSideEndpoints(t, router.URL, webdriverSessionID, webdriverParts.UpstreamSessionID)
	requireRecordedPath(t, webdriverBackend, "GET /vnc/"+webdriverParts.UpstreamSessionID)
	requireRecordedPath(t, webdriverBackend, "GET /devtools/"+webdriverParts.UpstreamSessionID+"/page")
	requireRecordedPath(t, webdriverBackend, "GET /video/"+webdriverParts.UpstreamSessionID+".mp4")
	requireRecordedPath(t, webdriverBackend, "GET /logs/"+webdriverParts.UpstreamSessionID)
	requireRecordedPath(t, webdriverBackend, "GET /download/"+webdriverParts.UpstreamSessionID+"/artifact.txt")
	requireRecordedPath(t, webdriverBackend, "GET /downloads/"+webdriverParts.UpstreamSessionID+"/artifact.txt")
	requireRecordedPath(t, webdriverBackend, "GET /clipboard/"+webdriverParts.UpstreamSessionID)

	exercisePlaywrightSideEndpoints(t, router.URL, playwrightSessionID)
	requireRecordedPath(t, playwrightBackend, "GET /vnc/"+playwrightSessionID)
	requireRecordedPath(t, playwrightBackend, "GET /devtools/"+playwrightSessionID+"/page")
	requireRecordedPath(t, playwrightBackend, "GET /video/"+playwrightSessionID+".mp4")
	requireRecordedPath(t, playwrightBackend, "GET /logs/"+playwrightSessionID)
	requireRecordedPath(t, playwrightBackend, "GET /download/"+playwrightSessionID+"/artifact.txt")
	requireRecordedPath(t, playwrightBackend, "GET /downloads/"+playwrightSessionID+"/artifact.txt")
	requireRecordedPath(t, playwrightBackend, "GET /clipboard/"+playwrightSessionID)

	requestOK(t, http.MethodGet, router.URL+"/history/settings", "", basicAuth)
	requireRecordedPath(t, webdriverBackend, "GET /history/settings")

	assertHostInfo(t, router.URL, webdriverSessionID, webdriverBackend.URL(), 2)
	assertHostInfo(t, router.URL, playwrightSessionID, playwrightBackend.URL(), 3)
	assertStatus(t, router.URL, 2)
	assertQuota(t, router.URL)
	assertSanitizedConfig(t, router.URL, 2)
}

func TestSessionIDsRouteAcrossRouterInstances(t *testing.T) {
	backend := newFakeSelenwright(t, "sw-shared")
	defer backend.Close()

	cfg := testRouterConfig(backendNode{
		ID:        "sw-shared",
		Endpoint:  backend.URL(),
		Region:    "eu",
		Weight:    1,
		Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
	})
	routerA := newRouter(t, cfg)
	defer routerA.Close()
	routerB := newRouter(t, cfg)
	defer routerB.Close()

	webdriverSessionID := createWebDriverSession(t, routerA.URL, "eu")
	webdriverParts := decodeSessionID(t, webdriverSessionID)
	requestOK(t, http.MethodPost, routerB.URL+"/wd/hub/session/"+webdriverSessionID+"/url", `{"url":"https://example.test"}`, basicAuth)
	requireRecordedPath(t, backend, "POST /wd/hub/session/"+webdriverParts.UpstreamSessionID+"/url")

	playwrightSessionID := openPlaywrightSession(t, routerA.URL, "/playwright/chrome/stable")
	requestOK(t, http.MethodGet, routerB.URL+"/logs/"+playwrightSessionID, "", basicAuth)
	requireRecordedPath(t, backend, "GET /logs/"+playwrightSessionID)
}

func TestConfigReloadIsAtomicAndDropsRemovedBackends(t *testing.T) {
	removedBackend := newFakeSelenwright(t, "sw-removed")
	defer removedBackend.Close()
	oldBackend := newFakeSelenwright(t, "sw-old")
	defer oldBackend.Close()
	reloadedBackend := newFakeSelenwright(t, "sw-reloaded")
	defer reloadedBackend.Close()

	configPath := writeConfig(t, testRouterConfig(
		backendNode{
			ID:        "sw-removed",
			Endpoint:  removedBackend.URL(),
			Region:    "eu",
			Weight:    1,
			Protocols: []config.Protocol{config.ProtocolWebDriver},
		},
		backendNode{
			ID:        "sw-stable",
			Endpoint:  oldBackend.URL(),
			Region:    "us",
			Weight:    1,
			Protocols: []config.Protocol{config.ProtocolWebDriver},
		},
	))
	handler := newReloadingHandler(t, configPath)
	router := httptest.NewServer(handler)
	defer router.Close()

	removedSessionID := createWebDriverSession(t, router.URL, "eu")
	removedParts := decodeSessionID(t, removedSessionID)
	requireRecordedPath(t, removedBackend, "POST /wd/hub/session")

	if err := os.WriteFile(configPath, []byte(`{`), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	if err := handler.Reload(); err == nil {
		t.Fatal("Reload() error = nil, want invalid config error")
	}

	requestOK(t, http.MethodPost, router.URL+"/wd/hub/session/"+removedSessionID+"/url", `{"url":"https://still-old.example"}`, basicAuth)
	requireRecordedPath(t, removedBackend, "POST /wd/hub/session/"+removedParts.UpstreamSessionID+"/url")

	writeConfigAt(t, configPath, testRouterConfig(backendNode{
		ID:        "sw-stable",
		Endpoint:  reloadedBackend.URL(),
		Region:    "eu",
		Weight:    7,
		Protocols: []config.Protocol{config.ProtocolWebDriver},
	}))
	if err := handler.Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	resp, body := request(t, http.MethodPost, router.URL+"/wd/hub/session/"+removedSessionID+"/url", `{"url":"https://removed.example"}`, basicAuth)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("removed backend follow-up status = %d, want %d; body: %s", resp.StatusCode, http.StatusNotFound, body)
	}

	reloadedSessionID := createWebDriverSession(t, router.URL, "eu")
	reloadedParts := decodeSessionID(t, reloadedSessionID)
	if !strings.HasPrefix(reloadedParts.UpstreamSessionID, "sw-reloaded-webdriver-") {
		t.Fatalf("reloaded upstream session = %q, want sw-reloaded backend", reloadedParts.UpstreamSessionID)
	}
	requireRecordedPath(t, reloadedBackend, "POST /wd/hub/session")
	assertHostInfo(t, router.URL, reloadedSessionID, reloadedBackend.URL(), 7)
}

type backendNode struct {
	ID        string
	Endpoint  string
	Region    string
	Weight    int
	Protocols []config.Protocol
}

func testRouterConfig(nodes ...backendNode) config.Config {
	pools := make([]config.BackendPool, 0, len(nodes))
	for _, node := range nodes {
		pools = append(pools, config.BackendPool{
			ID:        node.ID,
			Endpoint:  node.Endpoint,
			Region:    node.Region,
			Weight:    node.Weight,
			Protocols: node.Protocols,
			Health:    config.HealthPolicy{Enabled: true, FailureThreshold: 2, Cooldown: "1s"},
		})
	}
	return config.Config{
		Version: config.Version,
		Users: []config.User{{
			Name:        "alice",
			PasswordRef: "env:ALICE_PASSWORD",
			Quota:       config.Quota{MaxSessions: 4},
		}},
		Guest: &config.Guest{Quota: config.Quota{MaxSessions: 1}},
		Catalog: config.Catalog{Browsers: []config.Browser{{
			Name:      "chrome",
			Versions:  []string{"stable"},
			Platforms: []string{"linux"},
			Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
		}}},
		BackendPools: pools,
		Admin:        config.Admin{TokenRef: "env:GRIDLANE_ADMIN_TOKEN"},
	}
}

func newRouter(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()
	return httptest.NewServer(newReloadingHandler(t, writeConfig(t, cfg)))
}

func newReloadingHandler(t *testing.T, configPath string) *app.ReloadingHandler {
	t.Helper()
	t.Setenv("ALICE_PASSWORD", "wonderland")
	t.Setenv("GRIDLANE_ADMIN_TOKEN", "root-token")

	handler, err := app.NewReloadingHandler(app.Options{
		ConfigPath:            configPath,
		SessionAttemptTimeout: time.Second,
		ProxyTimeout:          time.Second,
	})
	if err != nil {
		t.Fatalf("NewReloadingHandler() error = %v", err)
	}
	return handler
}

func writeConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	path := t.TempDir() + "/router.json"
	writeConfigAt(t, path, cfg)
	return path
}

func writeConfigAt(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

type authMode int

const (
	noAuth authMode = iota
	basicAuth
	adminAuth
)

func requestOK(t *testing.T, method string, rawURL string, body string, auth authMode) []byte {
	t.Helper()
	resp, payload := request(t, method, rawURL, body, auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s status = %d, want %d; body: %s", method, rawURL, resp.StatusCode, http.StatusOK, payload)
	}
	return payload
}

func request(t *testing.T, method string, rawURL string, body string, mode authMode) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch mode {
	case basicAuth:
		req.SetBasicAuth("alice", "wonderland")
	case adminAuth:
		req.Header.Set(auth.HeaderAdminToken, "root-token")
	case noAuth:
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, payload
}

func createWebDriverSession(t *testing.T, routerURL string, region string) string {
	t.Helper()
	capabilities := map[string]any{
		"browserName":    "chrome",
		"browserVersion": "stable",
		"platformName":   "linux",
	}
	if region != "" {
		capabilities["gridlane:region"] = region
	}
	payload, err := json.Marshal(map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": capabilities,
		},
	})
	if err != nil {
		t.Fatalf("marshal webdriver payload: %v", err)
	}
	body := requestOK(t, http.MethodPost, routerURL+"/wd/hub/session", string(payload), basicAuth)

	var session struct {
		SessionID string `json:"sessionId"`
		Value     struct {
			SessionID string `json:"sessionId"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatalf("decode webdriver new session response: %v", err)
	}
	if session.Value.SessionID != "" {
		return session.Value.SessionID
	}
	if session.SessionID != "" {
		return session.SessionID
	}
	t.Fatalf("new session response did not contain a session id: %s", body)
	return ""
}

func openPlaywrightSession(t *testing.T, routerURL string, path string) string {
	t.Helper()
	parsed, err := url.Parse(routerURL)
	if err != nil {
		t.Fatalf("parse router URL: %v", err)
	}
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatalf("dial router websocket: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\n", path)
	_, _ = fmt.Fprintf(conn, "Host: %s\r\n", parsed.Host)
	_, _ = fmt.Fprintf(conn, "Connection: Upgrade\r\n")
	_, _ = fmt.Fprintf(conn, "Upgrade: websocket\r\n")
	_, _ = fmt.Fprintf(conn, "Sec-WebSocket-Version: 13\r\n")
	_, _ = fmt.Fprintf(conn, "Sec-WebSocket-Key: %s\r\n", key)
	_, _ = fmt.Fprintf(conn, "Sec-WebSocket-Protocol: playwright-json\r\n")
	_, _ = fmt.Fprintf(conn, "Authorization: Basic %s\r\n", base64.StdEncoding.EncodeToString([]byte("alice:wonderland")))
	_, _ = fmt.Fprintf(conn, "\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read websocket upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("websocket upgrade status = %d, want %d; body: %s", resp.StatusCode, http.StatusSwitchingProtocols, body)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "playwright-json" {
		t.Fatalf("Sec-WebSocket-Protocol = %q, want playwright-json", got)
	}
	sessionID := resp.Header.Get("X-Selenwright-Session-ID")
	if sessionID == "" {
		t.Fatal("X-Selenwright-Session-ID header is empty")
	}
	return sessionID
}

func exerciseWebDriverSideEndpoints(t *testing.T, routerURL string, publicID string, upstreamID string) {
	t.Helper()
	for _, path := range []string{
		"/vnc/" + publicID,
		"/devtools/" + publicID + "/page",
		"/video/" + publicID,
		"/logs/" + publicID,
		"/download/" + publicID + "/artifact.txt",
		"/downloads/" + publicID + "/artifact.txt",
		"/clipboard/" + publicID,
	} {
		requestOK(t, http.MethodGet, routerURL+path, "", basicAuth)
	}
	if upstreamID == "" {
		t.Fatal("upstreamID is empty")
	}
}

func exercisePlaywrightSideEndpoints(t *testing.T, routerURL string, publicID string) {
	t.Helper()
	for _, path := range []string{
		"/vnc/" + publicID,
		"/devtools/" + publicID + "/page",
		"/video/" + publicID,
		"/logs/" + publicID,
		"/download/" + publicID + "/artifact.txt",
		"/downloads/" + publicID + "/artifact.txt",
		"/clipboard/" + publicID,
	} {
		requestOK(t, http.MethodGet, routerURL+path, "", basicAuth)
	}
}

func assertHostInfo(t *testing.T, routerURL string, publicID string, backendURL string, wantWeight int) {
	t.Helper()
	body := requestOK(t, http.MethodGet, routerURL+"/host/"+publicID, "", basicAuth)
	var host struct {
		Name  string `json:"Name"`
		Port  int    `json:"Port"`
		Count int    `json:"Count"`
	}
	if err := json.Unmarshal(body, &host); err != nil {
		t.Fatalf("decode host info: %v", err)
	}
	backend, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	if host.Name != backend.Hostname() {
		t.Fatalf("host info name = %q, want %q", host.Name, backend.Hostname())
	}
	if fmt.Sprint(host.Port) != backend.Port() {
		t.Fatalf("host info port = %d, want %s", host.Port, backend.Port())
	}
	if host.Count != wantWeight {
		t.Fatalf("host info weight = %d, want %d", host.Count, wantWeight)
	}
}

func assertStatus(t *testing.T, routerURL string, wantBackends int) {
	t.Helper()
	body := requestOK(t, http.MethodGet, routerURL+"/status", "", noAuth)
	var snapshot health.PublicSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if snapshot.Service != "gridlane" {
		t.Fatalf("status service = %q, want gridlane", snapshot.Service)
	}
	if snapshot.Status != "ok" {
		t.Fatalf("status = %q, want ok", snapshot.Status)
	}
	if snapshot.BackendCount != wantBackends {
		t.Fatalf("status backend_count = %d, want %d", snapshot.BackendCount, wantBackends)
	}
	if snapshot.AvailableCount != wantBackends {
		t.Fatalf("status available_count = %d, want %d", snapshot.AvailableCount, wantBackends)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode status raw: %v", err)
	}
	for _, forbidden := range []string{"backends", "endpoint", "region", "protocols", "failures", "failure_threshold", "unhealthy_until"} {
		if _, leaked := raw[forbidden]; leaked {
			t.Fatalf("/status must not expose %q; got body: %s", forbidden, body)
		}
	}
}

func assertQuota(t *testing.T, routerURL string) {
	t.Helper()
	body := requestOK(t, http.MethodGet, routerURL+"/quota", "", basicAuth)
	var quota struct {
		Service string `json:"service"`
		Users   []struct {
			Name  string `json:"name"`
			Quota struct {
				MaxSessions int `json:"max_sessions"`
			} `json:"quota"`
		} `json:"users"`
	}
	if err := json.Unmarshal(body, &quota); err != nil {
		t.Fatalf("decode quota: %v", err)
	}
	if quota.Service != "gridlane" || len(quota.Users) != 1 || quota.Users[0].Name != "alice" || quota.Users[0].Quota.MaxSessions != 4 {
		t.Fatalf("unexpected quota response: %s", body)
	}
}

func assertSanitizedConfig(t *testing.T, routerURL string, wantBackends int) {
	t.Helper()
	body := requestOK(t, http.MethodGet, routerURL+"/config", "", adminAuth)
	for _, forbidden := range []string{"wonderland", "root-token", "env:ALICE_PASSWORD", "env:GRIDLANE_ADMIN_TOKEN"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("config response leaked %q: %s", forbidden, body)
		}
	}
	var view struct {
		Service      string `json:"service"`
		BackendPools []any  `json:"backend_pools"`
		Admin        struct {
			TokenConfigured bool `json:"token_configured"`
		} `json:"admin"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if view.Service != "gridlane" || len(view.BackendPools) != wantBackends || !view.Admin.TokenConfigured {
		t.Fatalf("unexpected config response: %s", body)
	}
}

func decodeSessionID(t *testing.T, publicID string) sessionid.Parts {
	t.Helper()
	parts, err := sessionid.Decode(publicID)
	if err != nil {
		t.Fatalf("decode session ID %q: %v", publicID, err)
	}
	return parts
}

type fakeSelenwright struct {
	id      string
	server  *httptest.Server
	mu      sync.Mutex
	next    int
	records []requestRecord
}

type requestRecord struct {
	Method            string
	URI               string
	Authorization     string
	ExternalSessionID string
	Subprotocol       string
}

func newFakeSelenwright(t *testing.T, id string) *fakeSelenwright {
	t.Helper()
	backend := &fakeSelenwright{id: id}
	backend.server = httptest.NewServer(http.HandlerFunc(backend.handle))
	return backend
}

func (b *fakeSelenwright) URL() string {
	return b.server.URL
}

func (b *fakeSelenwright) Close() {
	b.server.Close()
}

func (b *fakeSelenwright) handle(w http.ResponseWriter, r *http.Request) {
	b.record(r)
	switch {
	case isWebDriverNewSession(r):
		sessionID := b.nextWebDriverSessionID()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", backendURL(r)+r.URL.Path+"/"+sessionID)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": map[string]any{
				"sessionId": sessionID,
				"capabilities": map[string]any{
					"browserName":      "chrome",
					"gridlane:backend": b.id,
				},
			},
		})
	case strings.HasPrefix(r.URL.Path, "/playwright/"):
		b.handlePlaywrightUpgrade(w, r)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value":   "ok",
			"backend": b.id,
			"path":    r.URL.Path,
		})
	}
}

func (b *fakeSelenwright) record(r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records = append(b.records, requestRecord{
		Method:            r.Method,
		URI:               r.URL.RequestURI(),
		Authorization:     r.Header.Get("Authorization"),
		ExternalSessionID: r.Header.Get("X-Selenwright-External-Session-ID"),
		Subprotocol:       r.Header.Get("Sec-WebSocket-Protocol"),
	})
}

func (b *fakeSelenwright) nextWebDriverSessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	return fmt.Sprintf("%s-webdriver-%d", b.id, b.next)
}

func (b *fakeSelenwright) handlePlaywrightUpgrade(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}
	defer func() {
		_ = conn.Close()
	}()

	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
	_, _ = fmt.Fprintf(rw, "Upgrade: websocket\r\n")
	_, _ = fmt.Fprintf(rw, "Connection: Upgrade\r\n")
	_, _ = fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n", websocketAccept(r.Header.Get("Sec-WebSocket-Key")))
	if protocol := r.Header.Get("Sec-WebSocket-Protocol"); protocol != "" {
		_, _ = fmt.Fprintf(rw, "Sec-WebSocket-Protocol: %s\r\n", protocol)
	}
	_, _ = fmt.Fprintf(rw, "X-Selenwright-Session-ID: %s\r\n", r.Header.Get("X-Selenwright-External-Session-ID"))
	_, _ = fmt.Fprintf(rw, "\r\n")
	if err := rw.Flush(); err != nil {
		return
	}
	_, _ = conn.Write([]byte{0x81, 0x03, 'o', 'k', '\n'})
	_, _ = conn.Write([]byte{0x88, 0x00})
}

func (b *fakeSelenwright) Records() []requestRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]requestRecord(nil), b.records...)
}

func isWebDriverNewSession(r *http.Request) bool {
	return r.Method == http.MethodPost && (r.URL.Path == "/wd/hub/session" || r.URL.Path == "/session")
}

func requireRecordedPath(t *testing.T, backend *fakeSelenwright, want string) {
	t.Helper()
	for _, record := range backend.Records() {
		if record.Method+" "+record.URI == want {
			if strings.HasPrefix(record.URI, "/playwright/") && record.Subprotocol != "playwright-json" {
				t.Fatalf("playwright subprotocol for %s = %q, want playwright-json", want, record.Subprotocol)
			}
			if record.Authorization != "" {
				t.Fatalf("upstream Authorization for %s = %q, want stripped client auth", want, record.Authorization)
			}
			return
		}
	}
	t.Fatalf("backend %s did not record %q; records: %#v", backend.id, want, backend.Records())
}

func requirePlaywrightExternalID(t *testing.T, backend *fakeSelenwright, want string) {
	t.Helper()
	for _, record := range backend.Records() {
		if strings.HasPrefix(record.URI, "/playwright/") {
			if record.ExternalSessionID != want {
				t.Fatalf("playwright external session id = %q, want %q", record.ExternalSessionID, want)
			}
			return
		}
	}
	t.Fatalf("backend %s did not record a playwright request", backend.id)
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func backendURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
