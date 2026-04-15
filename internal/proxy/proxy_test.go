package proxy

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gridlane/internal/config"
	"gridlane/internal/sessionid"
)

func TestCreateSessionRewritesJSONWireSessionIDAndLocation(t *testing.T) {
	var gotPath string
	var gotAuthorization string
	var gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", backendURL(r)+"/wd/hub/session/upstream-jsonwire")
		_, _ = w.Write([]byte(`{"sessionId":"upstream-jsonwire","status":0,"value":{"browserName":"chrome"}}`))
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})
	req := httptest.NewRequest(http.MethodPost, "http://gridlane.test/wd/hub/session", strings.NewReader(`{
		"sessionId": "client-sent-id",
		"desiredCapabilities": {"browserName":"chrome","version":"stable"}
	}`))
	req.SetBasicAuth("alice", "wonderland")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotPath != "/wd/hub/session" {
		t.Fatalf("upstream path = %q, want /wd/hub/session", gotPath)
	}
	if gotAuthorization != "" {
		t.Fatalf("upstream Authorization = %q, want stripped client auth", gotAuthorization)
	}
	if strings.Contains(gotBody, "sessionId") {
		t.Fatalf("upstream request body still contains sessionId: %s", gotBody)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	publicSessionID, ok := payload["sessionId"].(string)
	if !ok {
		t.Fatalf("sessionId = %T, want string", payload["sessionId"])
	}
	parts, err := sessionid.Decode(publicSessionID)
	if err != nil {
		t.Fatalf("decode public session id: %v", err)
	}
	if parts.UpstreamSessionID != "upstream-jsonwire" {
		t.Fatalf("upstream session id = %q, want upstream-jsonwire", parts.UpstreamSessionID)
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "/wd/hub/session/"+publicSessionID) {
		t.Fatalf("Location = %q, want public session id", location)
	}
	if !strings.HasPrefix(location, "http://gridlane.test/") {
		t.Fatalf("Location = %q, want gridlane host", location)
	}
}

func TestCreateSessionRewritesW3CValueSessionID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session" {
			t.Fatalf("upstream path = %q, want /session", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":{"sessionId":"upstream-w3c","capabilities":{"browserName":"chrome"}}}`))
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})
	req := httptest.NewRequest(http.MethodPost, "http://gridlane.test/session", strings.NewReader(`{
		"capabilities": {"alwaysMatch": {"browserName":"chrome","browserVersion":"stable"}}
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Value struct {
			SessionID string `json:"sessionId"`
		} `json:"value"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	parts, err := sessionid.Decode(payload.Value.SessionID)
	if err != nil {
		t.Fatalf("decode public session id: %v", err)
	}
	if parts.UpstreamSessionID != "upstream-w3c" {
		t.Fatalf("upstream session id = %q, want upstream-w3c", parts.UpstreamSessionID)
	}
}

func TestCreateSessionDoesNotTrustForwardedProtoForLocation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", backendURL(r)+"/wd/hub/session/upstream-location")
		_, _ = w.Write([]byte(`{"value":{"sessionId":"upstream-location","capabilities":{"browserName":"chrome"}}}`))
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})
	req := httptest.NewRequest(http.MethodPost, "http://gridlane.test/wd/hub/session", strings.NewReader(`{
		"desiredCapabilities": {"browserName":"chrome","version":"stable"}
	}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "http://gridlane.test/") {
		t.Fatalf("Location = %q, want request scheme without trusting X-Forwarded-Proto", location)
	}
}

func TestCreateSessionRejectsBadJSONBeforeCallingBackend(t *testing.T) {
	called := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})
	req := httptest.NewRequest(http.MethodPost, "http://gridlane.test/wd/hub/session", strings.NewReader(`{`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if called {
		t.Fatal("backend was called for bad JSON")
	}
}

func TestCreateSessionRejectsUnsupportedBrowser(t *testing.T) {
	called := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})
	req := httptest.NewRequest(http.MethodPost, "http://gridlane.test/wd/hub/session", strings.NewReader(`{
		"desiredCapabilities": {"browserName":"firefox"}
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if called {
		t.Fatal("backend was called for unsupported browser")
	}
}

func TestCreateSessionReportsBackendFailure(t *testing.T) {
	health := &trackingHealth{available: map[string]bool{"sw-a": true}}
	metrics := &trackingMetrics{}
	handler := newTestHandler(t, testConfig("http://127.0.0.1:4444"), Options{
		Health:    health,
		Metrics:   metrics,
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }),
	})
	req := httptest.NewRequest(http.MethodPost, "http://gridlane.test/wd/hub/session", strings.NewReader(`{
		"desiredCapabilities": {"browserName":"chrome"}
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if health.failures["sw-a"] != 1 {
		t.Fatalf("failure count = %d, want 1", health.failures["sw-a"])
	}
	if got := metrics.proxyRequests["webdriver/sw-a/error"]; got != 1 {
		t.Fatalf("proxy error metric = %d, want 1", got)
	}
}

func TestProxyWebDriverSessionStripsRouteAndRequestSessionID(t *testing.T) {
	publicSessionID := publicSessionIDFor(t, "sw-a", "upstream-followup")
	var gotPath string
	var gotBody string
	var gotAuthorization string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		gotBody = string(body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{
		Credentials: CredentialStore{
			"sw-a": {Username: "backend-user", Password: "backend-password"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "http://gridlane.test/wd/hub/session/"+publicSessionID+"/url", strings.NewReader(`{
		"sessionId": "`+publicSessionID+`",
		"url": "https://example.test"
	}`))
	req.SetBasicAuth("alice", "wonderland")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotPath != "/wd/hub/session/upstream-followup/url" {
		t.Fatalf("upstream path = %q, want route-stripped path", gotPath)
	}
	if strings.Contains(gotBody, "sessionId") {
		t.Fatalf("upstream request body still contains sessionId: %s", gotBody)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("backend-user:backend-password"))
	if gotAuthorization != wantAuth {
		t.Fatalf("upstream Authorization = %q, want backend credentials", gotAuthorization)
	}
}

func TestProxySideEndpointStripsRoute(t *testing.T) {
	publicSessionID := publicSessionIDFor(t, "sw-a", "upstream-side")
	var gotPaths []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})

	for _, path := range []string{
		"/download/" + publicSessionID + "/report.txt",
		"/video/" + publicSessionID,
		"/logs/" + publicSessionID,
		"/clipboard/" + publicSessionID,
		"/devtools/" + publicSessionID + "/page",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://gridlane.test"+path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status code = %d, want %d; body: %s", path, rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	want := []string{
		"/download/upstream-side/report.txt",
		"/video/upstream-side.mp4",
		"/logs/upstream-side",
		"/clipboard/upstream-side",
		"/devtools/upstream-side/page",
	}
	if len(gotPaths) != len(want) {
		t.Fatalf("got %d upstream paths, want %d: %#v", len(gotPaths), len(want), gotPaths)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Fatalf("upstream path[%d] = %q, want %q", i, gotPaths[i], want[i])
		}
	}
}

func TestProxyPlaywrightWebSocketUpgradePreservesHeadersQuerySubprotocolAndExternalID(t *testing.T) {
	type upgradeRequest struct {
		path              string
		rawQuery          string
		trace             string
		subprotocol       string
		externalSessionID string
	}
	seen := make(chan upgradeRequest, 1)
	upstreamClosed := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalSessionID := r.Header.Get(headerSelenwrightExternalSessionID)
		seen <- upgradeRequest{
			path:              r.URL.Path,
			rawQuery:          r.URL.RawQuery,
			trace:             r.Header.Get("X-Trace"),
			subprotocol:       r.Header.Get("Sec-WebSocket-Protocol"),
			externalSessionID: externalSessionID,
		}

		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack upstream websocket: %v", err)
			return
		}
		defer func() {
			_ = conn.Close()
			upstreamClosed <- struct{}{}
		}()

		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
		_, _ = fmt.Fprintf(rw, "Upgrade: websocket\r\n")
		_, _ = fmt.Fprintf(rw, "Connection: Upgrade\r\n")
		_, _ = fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n", websocketAccept(r.Header.Get("Sec-WebSocket-Key")))
		_, _ = fmt.Fprintf(rw, "Sec-WebSocket-Protocol: %s\r\n", r.Header.Get("Sec-WebSocket-Protocol"))
		_, _ = fmt.Fprintf(rw, "%s: %s\r\n", "X-Selenwright-Session-ID", externalSessionID)
		_, _ = fmt.Fprintf(rw, "\r\n")
		if err := rw.Flush(); err != nil {
			t.Errorf("flush upstream websocket handshake: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})
	router := httptest.NewServer(handler)
	defer router.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(router.URL, "http://"))
	if err != nil {
		t.Fatalf("dial gridlane: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	key := base64.StdEncoding.EncodeToString([]byte("gridlane-ws-key!!"))
	_, _ = fmt.Fprintf(conn, "GET /playwright/chrome/stable?trace=1 HTTP/1.1\r\n")
	_, _ = fmt.Fprintf(conn, "Host: %s\r\n", strings.TrimPrefix(router.URL, "http://"))
	_, _ = fmt.Fprintf(conn, "Connection: Upgrade\r\n")
	_, _ = fmt.Fprintf(conn, "Upgrade: websocket\r\n")
	_, _ = fmt.Fprintf(conn, "Sec-WebSocket-Version: 13\r\n")
	_, _ = fmt.Fprintf(conn, "Sec-WebSocket-Key: %s\r\n", key)
	_, _ = fmt.Fprintf(conn, "Sec-WebSocket-Protocol: playwright-json\r\n")
	_, _ = fmt.Fprintf(conn, "X-Trace: keep-me\r\n")
	_, _ = fmt.Fprintf(conn, "\r\n")

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read websocket response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status code = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "playwright-json" {
		t.Fatalf("Sec-WebSocket-Protocol = %q, want playwright-json", got)
	}

	gotSessionID := resp.Header.Get("X-Selenwright-Session-ID")
	parts, err := sessionid.Decode(gotSessionID)
	if err != nil {
		t.Fatalf("decode returned session id %q: %v", gotSessionID, err)
	}
	if !strings.HasPrefix(parts.UpstreamSessionID, playwrightExternalSessionPrefix) {
		t.Fatalf("upstream/external session id = %q, want %s prefix", parts.UpstreamSessionID, playwrightExternalSessionPrefix)
	}

	var got upgradeRequest
	select {
	case got = <-seen:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream websocket request")
	}
	if got.path != "/playwright/chrome/stable" {
		t.Fatalf("upstream path = %q, want /playwright/chrome/stable", got.path)
	}
	if got.rawQuery != "trace=1" {
		t.Fatalf("upstream query = %q, want trace=1", got.rawQuery)
	}
	if got.trace != "keep-me" {
		t.Fatalf("X-Trace = %q, want keep-me", got.trace)
	}
	if got.subprotocol != "playwright-json" {
		t.Fatalf("upstream subprotocol = %q, want playwright-json", got.subprotocol)
	}
	if got.externalSessionID != gotSessionID {
		t.Fatalf("upstream external session id = %q, response session id = %q", got.externalSessionID, gotSessionID)
	}

	_ = conn.Close()
	select {
	case <-upstreamClosed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream websocket close")
	}
}

func TestProxyPlaywrightSideEndpointKeepsPublicSessionID(t *testing.T) {
	publicSessionID := publicPlaywrightSessionIDFor(t, "sw-a", "external")
	var gotPaths []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})

	for _, path := range []string{
		"/logs/" + publicSessionID,
		"/download/" + publicSessionID + "/report.txt",
		"/downloads/" + publicSessionID + "/report.txt",
		"/video/" + publicSessionID,
		"/devtools/" + publicSessionID + "/page",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://gridlane.test"+path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status code = %d, want %d; body: %s", path, rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	want := []string{
		"/logs/" + publicSessionID,
		"/download/" + publicSessionID + "/report.txt",
		"/downloads/" + publicSessionID + "/report.txt",
		"/video/" + publicSessionID + ".mp4",
		"/devtools/" + publicSessionID + "/page",
	}
	if len(gotPaths) != len(want) {
		t.Fatalf("got %d upstream paths, want %d: %#v", len(gotPaths), len(want), gotPaths)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Fatalf("upstream path[%d] = %q, want %q", i, gotPaths[i], want[i])
		}
	}
}

func TestHostInfoUsesBackendFromRouteToken(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	handler := newTestHandler(t, testConfig(backend.URL), Options{})
	publicSessionID := publicSessionIDFor(t, "sw-a", "upstream-host")
	req := httptest.NewRequest(http.MethodGet, "http://gridlane.test/host/"+publicSessionID, nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var info HostInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode host info: %v", err)
	}
	if info.Name == "" {
		t.Fatal("host info name is empty")
	}
	if info.Port == 0 {
		t.Fatal("host info port is zero")
	}
	if info.Count != 3 {
		t.Fatalf("host info count = %d, want backend weight 3", info.Count)
	}
	if info.Username != "" || info.Password != "" {
		t.Fatalf("host info leaked credentials: %#v", info)
	}
}

func TestDefaultTransportHasProductionLimits(t *testing.T) {
	transport, ok := defaultTransport(5 * time.Minute).(*http.Transport)
	if !ok {
		t.Fatalf("defaultTransport() = %T, want *http.Transport", defaultTransport(5*time.Minute))
	}
	if transport.ResponseHeaderTimeout == 0 {
		t.Fatal("ResponseHeaderTimeout = 0, want bounded upstream header wait")
	}
	if transport.MaxConnsPerHost != defaultProxyMaxConnsPerHost {
		t.Fatalf("MaxConnsPerHost = %d, want %d", transport.MaxConnsPerHost, defaultProxyMaxConnsPerHost)
	}
	if transport.MaxResponseHeaderBytes != 1<<20 {
		t.Fatalf("MaxResponseHeaderBytes = %d, want 1MiB", transport.MaxResponseHeaderBytes)
	}
}

func testConfig(endpoint string) config.Config {
	return config.Config{
		Version: config.Version,
		Guest:   &config.Guest{Quota: config.Quota{MaxSessions: 1}},
		Catalog: config.Catalog{Browsers: []config.Browser{{
			Name:      "chrome",
			Versions:  []string{"stable"},
			Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
		}}},
		BackendPools: []config.BackendPool{{
			ID:        "sw-a",
			Endpoint:  endpoint,
			Region:    "local",
			Weight:    3,
			Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
		}},
	}
}

func newTestHandler(t *testing.T, cfg config.Config, opts Options) *Handler {
	t.Helper()
	opts.Config = cfg
	if opts.SessionAttemptTimeout == 0 {
		opts.SessionAttemptTimeout = time.Second
	}
	if opts.ProxyTimeout == 0 {
		opts.ProxyTimeout = time.Second
	}
	handler, err := NewHandler(opts)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func publicSessionIDFor(t *testing.T, backendID string, upstreamSessionID string) string {
	t.Helper()
	routeToken, err := sessionid.TokenForBackend(backendID)
	if err != nil {
		t.Fatalf("TokenForBackend() error = %v", err)
	}
	publicSessionID, err := sessionid.Encode(routeToken, upstreamSessionID)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	return publicSessionID
}

func publicPlaywrightSessionIDFor(t *testing.T, backendID string, upstreamSessionID string) string {
	t.Helper()
	return publicSessionIDFor(t, backendID, playwrightExternalSessionPrefix+upstreamSessionID)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

type trackingHealth struct {
	available map[string]bool
	failures  map[string]int
	successes map[string]int
}

type trackingMetrics struct {
	proxyRequests     map[string]int
	websocketSessions map[string]int
}

func (m *trackingMetrics) RecordProxyRequest(protocol string, backendID string, outcome string, _ time.Duration) {
	if m.proxyRequests == nil {
		m.proxyRequests = map[string]int{}
	}
	m.proxyRequests[protocol+"/"+backendID+"/"+outcome]++
}

func (m *trackingMetrics) RecordWebSocketSession(backendID string, event string) {
	if m.websocketSessions == nil {
		m.websocketSessions = map[string]int{}
	}
	m.websocketSessions[backendID+"/"+event]++
}

func (h *trackingHealth) Available(backendID string) bool {
	if h.available == nil {
		return true
	}
	return h.available[backendID]
}

func (h *trackingHealth) ReportSuccess(backendID string) {
	if h.successes == nil {
		h.successes = map[string]int{}
	}
	h.successes[backendID]++
}

func (h *trackingHealth) ReportFailure(backendID string) {
	if h.failures == nil {
		h.failures = map[string]int{}
	}
	h.failures[backendID]++
}
