// Package proxy owns WebDriver HTTP reverse proxy behavior.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gridlane/internal/config"
	"gridlane/internal/routing"
	"gridlane/internal/sessionid"
	"gridlane/internal/sideroute"
)

const (
	maxProxyBodyBytes           = 8 << 20
	defaultProxyMaxConnsPerHost = 128

	// headerSelenwrightExternalSessionID is attached to the Playwright
	// WebSocket upgrade request so upstream selenwright uses the router-
	// supplied public session ID as its app.sessions key. Requires selenwright
	// with feat/accept-external-playwright-session-id merged — older
	// selenwright builds ignore the header and break side endpoints.
	headerSelenwrightExternalSessionID = "X-Selenwright-External-Session-ID"

	// headerSelenwrightSessionID is attached by gridlane to the 101
	// Switching Protocols response so Playwright clients can discover the
	// public session ID (encoded r1_<routeToken>_<external>) and address
	// side endpoints (/vnc, /video, /logs, ...) afterwards. selenwright
	// itself does not emit this header — it is a router↔client convention.
	headerSelenwrightSessionID = "X-Selenwright-Session-ID"

	playwrightExternalSessionPrefix = "pw_"
)

type Health interface {
	Available(backendID string) bool
	ReportSuccess(backendID string)
	ReportFailure(backendID string)
}

type Metrics interface {
	RecordProxyRequest(protocol string, backendID string, outcome string, duration time.Duration)
	RecordWebSocketSession(backendID string, event string)
}

type SecretResolver interface {
	Resolve(ref string) (string, error)
}

type BackendCredential struct {
	Username string
	Password string
}

type CredentialStore map[string]BackendCredential

func NewCredentialStore(pools []config.BackendPool, resolver SecretResolver) (CredentialStore, error) {
	store := CredentialStore{}
	for _, pool := range pools {
		if pool.Credentials == nil {
			continue
		}
		if resolver == nil {
			return nil, fmt.Errorf("backend credentials configured for %q but no resolver was provided", pool.ID)
		}
		username, err := resolver.Resolve(pool.Credentials.UsernameRef)
		if err != nil {
			return nil, fmt.Errorf("resolve backend username for %q: %w", pool.ID, err)
		}
		password, err := resolver.Resolve(pool.Credentials.PasswordRef)
		if err != nil {
			return nil, fmt.Errorf("resolve backend password for %q: %w", pool.ID, err)
		}
		store[pool.ID] = BackendCredential{Username: username, Password: password}
	}
	return store, nil
}

type Options struct {
	Config                config.Config
	Health                Health
	Credentials           CredentialStore
	SessionAttemptTimeout time.Duration
	ProxyTimeout          time.Duration
	Transport             http.RoundTripper
	Seed                  uint64
	Metrics               Metrics
}

type Handler struct {
	selector              *routing.Selector
	health                Health
	credentials           CredentialStore
	client                *http.Client
	transport             http.RoundTripper
	sessionAttemptTimeout time.Duration
	tokenToBackend        map[string]config.BackendPool
	pools                 []config.BackendPool
	metrics               Metrics
}

func NewHandler(opts Options) (*Handler, error) {
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	sessionAttemptTimeout := opts.SessionAttemptTimeout
	if sessionAttemptTimeout <= 0 {
		sessionAttemptTimeout = 30 * time.Second
	}
	proxyTimeout := opts.ProxyTimeout
	if proxyTimeout <= 0 {
		proxyTimeout = 5 * time.Minute
	}
	transport := opts.Transport
	if transport == nil {
		transport = defaultTransport(proxyTimeout)
	}

	tokenToBackend := make(map[string]config.BackendPool, len(opts.Config.BackendPools))
	for _, pool := range opts.Config.BackendPools {
		token, err := sessionid.TokenForBackend(pool.ID)
		if err != nil {
			return nil, err
		}
		if _, ok := tokenToBackend[token]; ok {
			return nil, fmt.Errorf("route token collision for backend %q", pool.ID)
		}
		tokenToBackend[token] = pool
	}

	credentials := opts.Credentials
	if credentials == nil {
		credentials = CredentialStore{}
	}

	return &Handler{
		selector:              routing.NewSelector(opts.Config.Catalog, opts.Config.BackendPools, opts.Seed),
		health:                opts.Health,
		credentials:           credentials,
		client:                &http.Client{Transport: transport, Timeout: proxyTimeout},
		transport:             transport,
		sessionAttemptTimeout: sessionAttemptTimeout,
		tokenToBackend:        tokenToBackend,
		pools:                 append([]config.BackendPool(nil), opts.Config.BackendPools...),
		metrics:               opts.Metrics,
	}, nil
}

func defaultTransport(proxyTimeout time.Duration) http.RoundTripper {
	responseHeaderTimeout := min(proxyTimeout, 30*time.Second)
	return &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           512,
		MaxIdleConnsPerHost:    64,
		MaxConnsPerHost:        defaultProxyMaxConnsPerHost,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		MaxResponseHeaderBytes: 1 << 20,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case isNewSessionPath(r.URL.Path):
		h.createSession(w, r)
	case strings.HasPrefix(r.URL.Path, "/wd/hub/session/") || strings.HasPrefix(r.URL.Path, "/session/"):
		h.proxyWebDriverSession(w, r)
	case strings.HasPrefix(r.URL.Path, "/playwright/"):
		h.proxyPlaywright(w, r)
	case strings.HasPrefix(r.URL.Path, "/host/"):
		h.hostInfo(w, r)
	case isSideEndpoint(r.URL.Path):
		h.proxySideEndpoint(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := readLimitedBody(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	candidates, err := routing.ParseWebDriverNewSession(bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	upstreamBody, err := stripSessionIDFromJSONBody(body, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	backend, _, err := h.selector.SelectFirst(candidates, h.health)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	upstreamPath := r.URL.Path
	ctx, cancel := context.WithTimeout(r.Context(), h.sessionAttemptTimeout)
	defer cancel()
	upstreamRequest, err := h.newUpstreamRequest(ctx, r, backend, upstreamPath, bytes.NewReader(upstreamBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	upstreamRequest.ContentLength = int64(len(upstreamBody))

	started := time.Now()
	resp, err := h.client.Do(upstreamRequest)
	if err != nil {
		h.reportFailure(backend.ID)
		h.recordProxyRequest(config.ProtocolWebDriver, backend.ID, "error", time.Since(started))
		http.Error(w, "upstream webdriver session request failed", http.StatusBadGateway)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := readLimitedBody(resp.Body)
	if err != nil {
		h.reportFailure(backend.ID)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	h.classifyAndRecord(config.ProtocolWebDriver, backend.ID, resp.StatusCode, started)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	routeToken, err := sessionid.TokenForBackend(backend.ID)
	if err != nil {
		h.reportFailure(backend.ID)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rewrittenBody, upstreamSessionID, publicSessionID, err := rewriteNewSessionResponse(respBody, routeToken)
	if err != nil {
		h.reportFailure(backend.ID)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	if location := resp.Header.Get("Location"); location != "" {
		w.Header().Set("Location", rewriteLocation(r, backend, location, upstreamSessionID, publicSessionID))
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rewrittenBody)
}

func (h *Handler) proxyWebDriverSession(w http.ResponseWriter, r *http.Request) {
	basePath, publicSessionID, remainder, ok := splitWebDriverSessionPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid webdriver session path", http.StatusBadRequest)
		return
	}
	backend, upstreamSessionID, err := h.routeSession(publicSessionID)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	upstreamPath := basePath + "/" + upstreamSessionID + remainder
	if err := sanitizeProxyRequestBody(r); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	h.reverseProxy(w, r, backend, upstreamPath, config.ProtocolWebDriver)
}

func (h *Handler) proxyPlaywright(w http.ResponseWriter, r *http.Request) {
	if !isUpgradeRequest(r) {
		http.Error(w, "playwright endpoint requires websocket upgrade", http.StatusBadRequest)
		return
	}
	routeRequest, err := routing.ParsePlaywrightPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	backend, err := h.selector.Select(routeRequest, h.health)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	routeToken, err := sessionid.TokenForBackend(backend.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	externalSessionID, err := newPlaywrightExternalSessionID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	publicSessionID, err := sessionid.Encode(routeToken, externalSessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.Header.Set(headerSelenwrightExternalSessionID, publicSessionID)
	h.recordWebSocketSession(backend.ID, "started")
	h.reverseProxyWithResponseSession(w, r, backend, r.URL.Path, config.ProtocolPlaywright, publicSessionID)
}

func (h *Handler) proxySideEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/history/settings" || strings.HasPrefix(r.URL.Path, "/history/settings/") {
		backend, err := h.defaultBackend()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if err := sanitizeProxyRequestBody(r); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		h.reverseProxy(w, r, backend, r.URL.Path, "")
		return
	}

	prefix, rest, ok := splitSidePath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid side endpoint path", http.StatusBadRequest)
		return
	}
	if rest == "" {
		backend, err := h.defaultBackend()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		h.reverseProxy(w, r, backend, r.URL.Path, "")
		return
	}

	sessionPart, remainder := firstPathSegment(rest)
	publicSessionID, suffix := trimArtifactSuffix(prefix, sessionPart)
	backend, upstreamSessionID, err := h.routeSession(publicSessionID)
	if err != nil {
		writeRouteError(w, err)
		return
	}

	upstreamSessionPart := upstreamSessionID + suffix
	protocol := config.ProtocolWebDriver
	if strings.HasPrefix(upstreamSessionID, playwrightExternalSessionPrefix) {
		// Playwright sessions are registered in upstream selenwright under the
		// full public ID (r1_<routeToken>_pw_<hex>) because we pass it through
		// X-Selenwright-External-Session-ID on the upgrade — so side endpoints
		// must address the upstream with the public ID, not the decoded
		// upstream part. Requires selenwright with
		// feat/accept-external-playwright-session-id merged.
		upstreamSessionPart = publicSessionID + suffix
		protocol = config.ProtocolPlaywright
	}
	if prefix == "/video/" && suffix == "" && remainder == "" {
		upstreamSessionPart += ".mp4"
	}
	upstreamPath := prefix + upstreamSessionPart + remainder
	if err := sanitizeProxyRequestBody(r); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	h.reverseProxy(w, r, backend, upstreamPath, protocol)
}

func (h *Handler) hostInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	publicSessionID := strings.TrimPrefix(r.URL.Path, "/host/")
	if publicSessionID == "" || strings.Contains(publicSessionID, "/") {
		http.Error(w, "invalid session ID", http.StatusBadRequest)
		return
	}
	backend, _, err := h.routeSession(publicSessionID)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	endpoint, err := url.Parse(backend.Endpoint)
	if err != nil {
		http.Error(w, "invalid backend endpoint", http.StatusBadGateway)
		return
	}
	port, _ := strconv.Atoi(endpoint.Port())
	if port == 0 {
		port = defaultPort(endpoint.Scheme)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(HostInfo{
		Name:     endpoint.Hostname(),
		Port:     port,
		Count:    backend.Weight,
		Username: "",
		Password: "",
	}); err != nil {
		slog.Warn("write host info response failed", "backend", backend.ID, "err", err)
	}
}

type HostInfo struct {
	Name     string `json:"Name"`
	Port     int    `json:"Port"`
	Count    int    `json:"Count"`
	Username string `json:"Username"`
	Password string `json:"Password"`
}

func (h *Handler) routeSession(publicSessionID string) (config.BackendPool, string, error) {
	parts, err := sessionid.Decode(publicSessionID)
	if err != nil {
		return config.BackendPool{}, "", routeError{status: http.StatusBadRequest, message: "invalid session ID"}
	}
	backend, ok := h.tokenToBackend[parts.RouteToken]
	if !ok {
		return config.BackendPool{}, "", routeError{status: http.StatusNotFound, message: "unknown backend route"}
	}
	return backend, parts.UpstreamSessionID, nil
}

func (h *Handler) defaultBackend() (config.BackendPool, error) {
	for _, pool := range h.pools {
		if h.health != nil && !h.health.Available(pool.ID) {
			continue
		}
		return pool, nil
	}
	return config.BackendPool{}, fmt.Errorf("no available backend")
}

func (h *Handler) newUpstreamRequest(ctx context.Context, original *http.Request, backend config.BackendPool, upstreamPath string, body io.Reader) (*http.Request, error) {
	target, err := upstreamURL(backend, original, upstreamPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, original.Method, target.String(), body)
	if err != nil {
		return nil, err
	}
	copyUpstreamRequestHeaders(req.Header, original.Header)
	h.applyBackendCredentials(req, backend.ID)
	req.Host = target.Host
	return req, nil
}

func (h *Handler) reverseProxy(w http.ResponseWriter, r *http.Request, backend config.BackendPool, upstreamPath string, protocol config.Protocol) {
	h.reverseProxyWithResponseSession(w, r, backend, upstreamPath, protocol, "")
}

// reverseProxyWithResponseSession adds a public session ID to the upgrade
// response on Playwright WS handoff. See [headerSelenwrightSessionID] — the
// header is the router↔client contract for discovering the encoded session
// id; upstream selenwright never emits it.
func (h *Handler) reverseProxyWithResponseSession(w http.ResponseWriter, r *http.Request, backend config.BackendPool, upstreamPath string, protocol config.Protocol, publicSessionID string) {
	target, err := url.Parse(backend.Endpoint)
	if err != nil {
		http.Error(w, "invalid backend endpoint", http.StatusBadGateway)
		return
	}
	started := time.Now()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = singleJoiningSlash(target.Path, upstreamPath)
			req.URL.RawPath = ""
			req.URL.RawQuery = joinQueries(target.RawQuery, r.URL.RawQuery)
			req.Host = target.Host
			req.Header.Del("Authorization")
			h.applyBackendCredentials(req, backend.ID)
		},
		Transport: h.transport,
		ModifyResponse: func(resp *http.Response) error {
			h.classifyAndRecord(protocol, backend.ID, resp.StatusCode, started)
			if resp.StatusCode == http.StatusSwitchingProtocols && protocol == config.ProtocolPlaywright {
				if publicSessionID != "" {
					resp.Header.Set(headerSelenwrightSessionID, publicSessionID)
				}
				h.recordWebSocketSession(backend.ID, "upgraded")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			h.reportFailure(backend.ID)
			h.recordProxyRequest(protocol, backend.ID, "error", time.Since(started))
			http.Error(w, "upstream proxy request failed", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

func (h *Handler) applyBackendCredentials(req *http.Request, backendID string) {
	credential, ok := h.credentials[backendID]
	if !ok {
		return
	}
	req.SetBasicAuth(credential.Username, credential.Password)
}

// classifyUpstreamStatus maps an HTTP status returned by the upstream to a
// Prometheus outcome label and decides whether it should degrade backend
// health. 5xx, 408/425/429 and upstream-side auth failures (401/403 — likely
// backend-credentials drift) trip the backend; other 4xx are the caller's
// problem ("client_error") and leave health untouched; 2xx/3xx are "success".
func classifyUpstreamStatus(code int) (outcome string, unhealthy bool) {
	switch {
	case code >= 500:
		return "failure", true
	case code == http.StatusRequestTimeout,
		code == http.StatusTooEarly,
		code == http.StatusTooManyRequests,
		code == http.StatusUnauthorized,
		code == http.StatusForbidden:
		return "failure", true
	case code >= 400:
		return "client_error", false
	default:
		return "success", false
	}
}

func (h *Handler) classifyAndRecord(protocol config.Protocol, backendID string, status int, started time.Time) {
	outcome, unhealthy := classifyUpstreamStatus(status)
	switch {
	case unhealthy:
		h.reportFailure(backendID)
	case outcome == "success":
		h.reportSuccess(backendID)
	}
	h.recordProxyRequest(protocol, backendID, outcome, time.Since(started))
}

func (h *Handler) reportSuccess(backendID string) {
	if h.health != nil {
		h.health.ReportSuccess(backendID)
	}
}

func (h *Handler) reportFailure(backendID string) {
	if h.health != nil {
		h.health.ReportFailure(backendID)
	}
}

func (h *Handler) recordProxyRequest(protocol config.Protocol, backendID string, outcome string, duration time.Duration) {
	if h.metrics == nil {
		return
	}
	protocolLabel := string(protocol)
	if protocolLabel == "" {
		protocolLabel = "side"
	}
	h.metrics.RecordProxyRequest(protocolLabel, backendID, outcome, duration)
}

func (h *Handler) recordWebSocketSession(backendID string, event string) {
	if h.metrics != nil {
		h.metrics.RecordWebSocketSession(backendID, event)
	}
}

func rewriteNewSessionResponse(body []byte, routeToken string) ([]byte, string, string, error) {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, "", "", fmt.Errorf("decode upstream new session response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, "", "", fmt.Errorf("upstream new session response must contain a single JSON object")
	}

	upstreamSessionID := ""
	if sessionID, ok := payload["sessionId"].(string); ok {
		upstreamSessionID = sessionID
	}
	if value, ok := payload["value"].(map[string]any); ok {
		if sessionID, ok := value["sessionId"].(string); ok {
			if upstreamSessionID != "" && upstreamSessionID != sessionID {
				return nil, "", "", fmt.Errorf("upstream response contains mismatched session ids")
			}
			upstreamSessionID = sessionID
		}
	}
	if upstreamSessionID == "" {
		return nil, "", "", fmt.Errorf("upstream response did not include a session id")
	}

	publicSessionID, err := sessionid.Encode(routeToken, upstreamSessionID)
	if err != nil {
		return nil, "", "", err
	}
	if _, ok := payload["sessionId"]; ok {
		payload["sessionId"] = publicSessionID
	}
	if value, ok := payload["value"].(map[string]any); ok {
		if _, ok := value["sessionId"]; ok {
			value["sessionId"] = publicSessionID
		}
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, "", "", err
	}
	return rewritten, upstreamSessionID, publicSessionID, nil
}

func stripSessionIDFromJSONBody(body []byte, strict bool) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	var payload any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		if strict {
			return nil, fmt.Errorf("decode JSON request body: %w", err)
		}
		return body, nil
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if strict {
			return nil, fmt.Errorf("request body must contain a single JSON object")
		}
		return body, nil
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return body, nil
	}
	if _, ok := object["sessionId"]; !ok {
		return body, nil
	}
	delete(object, "sessionId")
	return json.Marshal(object)
}

func sanitizeProxyRequestBody(r *http.Request) error {
	if r.Body == nil || r.Body == http.NoBody || isUpgradeRequest(r) {
		return nil
	}
	body, err := readLimitedBody(r.Body)
	if err != nil {
		return err
	}
	rewritten, err := stripSessionIDFromJSONBody(body, false)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	r.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return nil
}

func readLimitedBody(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, maxProxyBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxProxyBodyBytes {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxProxyBodyBytes)
	}
	return data, nil
}

func isNewSessionPath(path string) bool {
	return path == "/wd/hub/session" || path == "/session"
}

func splitWebDriverSessionPath(path string) (string, string, string, bool) {
	for _, prefix := range []string{"/wd/hub/session/", "/session/"} {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		if rest == "" {
			return "", "", "", false
		}
		session, remainder := firstPathSegment(rest)
		return strings.TrimSuffix(prefix, "/"), session, remainder, true
	}
	return "", "", "", false
}

func isSideEndpoint(path string) bool {
	return sideroute.IsSide(path)
}

func splitSidePath(path string) (string, string, bool) {
	return sideroute.MatchPrefix(path)
}

func firstPathSegment(path string) (string, string) {
	head, tail, ok := strings.Cut(path, "/")
	if !ok {
		return path, ""
	}
	return head, "/" + tail
}

func trimArtifactSuffix(prefix string, sessionPart string) (string, string) {
	switch {
	case prefix == "/video/" && strings.HasSuffix(sessionPart, ".mp4"):
		return strings.TrimSuffix(sessionPart, ".mp4"), ".mp4"
	case prefix == "/logs/" && strings.HasSuffix(sessionPart, ".log"):
		return strings.TrimSuffix(sessionPart, ".log"), ".log"
	default:
		return sessionPart, ""
	}
}

func newPlaywrightExternalSessionID() (string, error) {
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("generate playwright external session id: %w", err)
	}
	return playwrightExternalSessionPrefix + hex.EncodeToString(randomBytes), nil
}

func upstreamURL(backend config.BackendPool, original *http.Request, upstreamPath string) (*url.URL, error) {
	target, err := url.Parse(backend.Endpoint)
	if err != nil {
		return nil, err
	}
	out := *target
	out.Path = singleJoiningSlash(target.Path, upstreamPath)
	out.RawPath = ""
	out.RawQuery = joinQueries(target.RawQuery, original.URL.RawQuery)
	return &out, nil
}

func singleJoiningSlash(a string, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}

func joinQueries(a string, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "&" + b
	}
}

func rewriteLocation(original *http.Request, backend config.BackendPool, location string, upstreamSessionID string, publicSessionID string) string {
	rewritten := strings.ReplaceAll(location, upstreamSessionID, publicSessionID)
	parsed, err := url.Parse(rewritten)
	if err != nil || !parsed.IsAbs() {
		return rewritten
	}
	backendURL, err := url.Parse(backend.Endpoint)
	if err != nil {
		return rewritten
	}
	if parsed.Host != backendURL.Host {
		return rewritten
	}
	parsed.Scheme = externalScheme(original)
	parsed.Host = original.Host
	return parsed.String()
}

func externalScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func copyUpstreamRequestHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if shouldSkipRequestHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if isHopHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldSkipRequestHeader(key string) bool {
	return strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Host") || isHopHeader(key)
}

func isHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func isUpgradeRequest(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") || r.Header.Get("Upgrade") != ""
}

func defaultPort(scheme string) int {
	switch scheme {
	case "https":
		return 443
	case "http":
		return 80
	default:
		return 0
	}
}

type routeError struct {
	status  int
	message string
}

func (e routeError) Error() string {
	return e.message
}

func writeRouteError(w http.ResponseWriter, err error) {
	var routeErr routeError
	if errors.As(err, &routeErr) {
		http.Error(w, routeErr.message, routeErr.status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
