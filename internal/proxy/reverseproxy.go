package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"gridlane/internal/auth"
	"gridlane/internal/config"
)

// reverseProxy is the shared plumbing for non-Playwright upstream calls that
// do not need to expose a public session id on the 101 upgrade response.
// It delegates to reverseProxyWithResponseSession with an empty publicSessionID.
func (h *Handler) reverseProxy(w http.ResponseWriter, r *http.Request, backend config.BackendPool, upstreamPath string, protocol config.Protocol) {
	h.reverseProxyWithResponseSession(w, r, backend, upstreamPath, protocol, "")
}

// reverseProxyWithResponseSession runs an httputil.ReverseProxy configured
// for gridlane's upstream contract: session-scoped body sanitization has
// already happened by the time we get here; we only shape the request
// (scheme/host/path/credentials) and classify the response for health +
// metrics. When publicSessionID is non-empty and this is a Playwright
// upgrade, it is echoed to the client via X-Selenwright-Session-ID —
// upstream selenwright never sets that header itself.
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
			h.applyUpstreamIdentity(req, r.Context())
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
	h.applyUpstreamIdentity(req, original.Context())
	req.Host = target.Host
	return req, nil
}

// applyUpstreamIdentity stamps the resolved identity on the upstream request.
// Headers are scrubbed first so a stale value from header-copying (e.g. the
// original request somehow carried one despite the incoming spoof guard)
// cannot leak through. When the feature is disabled this is a no-op.
func (h *Handler) applyUpstreamIdentity(req *http.Request, ctx context.Context) {
	for _, header := range h.upstreamIdentity.StripHeaders() {
		req.Header.Del(header)
	}
	if !h.upstreamIdentity.Enabled() {
		return
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok || !identity.Allowed {
		// Authorization middleware should have rejected the request before
		// we ever got here; if it slipped through, emit no identity — failing
		// closed is safer than forwarding an ambiguous subject.
		return
	}
	subject := identity.Subject
	if subject == "" && identity.Guest {
		subject = "guest"
	}
	if subject != "" {
		req.Header.Set(h.upstreamIdentity.UserHeader, subject)
	}
	if identity.Admin && h.upstreamIdentity.AdminHeader != "" {
		req.Header.Set(h.upstreamIdentity.AdminHeader, "true")
	}
	if h.upstreamIdentity.RouterSecret != "" {
		req.Header.Set(headerSelenwrightRouterSecret, h.upstreamIdentity.RouterSecret)
	}
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
