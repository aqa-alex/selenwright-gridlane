// Package proxy owns WebDriver HTTP + Playwright WS reverse-proxy behavior
// for gridlane. The package is split by concern:
//   - proxy.go        — package constants, interfaces, Handler, NewHandler, dispatcher
//   - credentials.go  — backend BasicAuth secret store
//   - session.go      — WebDriver /session + /wd/hub/session flows
//   - playwright.go   — Playwright WS upgrade and external session id generation
//   - side.go         — /vnc, /video, /logs, /host, /history/settings and friends
//   - reverseproxy.go — httputil.ReverseProxy wiring, upstream request shaping,
//     health classification, header hygiene
//   - errors.go       — routeError used by session/side paths
package proxy

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"gridlane/internal/config"
	"gridlane/internal/routing"
	"gridlane/internal/sessionid"
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

	// headerSelenwrightRouterSecret carries the shared secret between gridlane
	// and selenwright's SourceTrust gate. Without it selenwright rejects any
	// trusted-proxy identity header — so a direct client cannot bypass gridlane
	// by stamping X-Forwarded-User themselves.
	headerSelenwrightRouterSecret = "X-Selenwright-Router-Secret"

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

// UpstreamIdentity is the resolved form of config.UpstreamIdentity: headers
// are pre-chosen and the router secret has already been read out of env:/file:
// so the hot path is an O(1) header set.
type UpstreamIdentity struct {
	UserHeader   string
	AdminHeader  string
	RouterSecret string
}

// Enabled reports whether identity propagation should run. Empty UserHeader
// leaves gridlane in legacy mode — no identity is forwarded.
func (u UpstreamIdentity) Enabled() bool { return u.UserHeader != "" }

// StripHeaders lists the header names that must be scrubbed off incoming
// client requests to stop clients from forging identity upstream.
func (u UpstreamIdentity) StripHeaders() []string {
	if !u.Enabled() {
		return nil
	}
	headers := []string{u.UserHeader, headerSelenwrightRouterSecret}
	if u.AdminHeader != "" {
		headers = append(headers, u.AdminHeader)
	}
	return headers
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
	UpstreamIdentity      UpstreamIdentity
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
	upstreamIdentity      UpstreamIdentity
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
		upstreamIdentity:      opts.UpstreamIdentity,
	}, nil
}

// UpstreamIdentity returns the configured identity-propagation settings so
// mux-level middleware can share the strip list.
func (h *Handler) UpstreamIdentityConfig() UpstreamIdentity { return h.upstreamIdentity }

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

// ServeHTTP dispatches to the per-concern handler files.
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
