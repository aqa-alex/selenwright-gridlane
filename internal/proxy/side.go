package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gridlane/internal/config"
	"gridlane/internal/sideroute"
)

// proxySideEndpoint routes /vnc, /devtools, /video, /logs, /download,
// /clipboard (session-addressed) plus /history/settings (non-session).
// For Playwright sessions the upstream path carries the public session id
// because selenwright stores the session under that value — see the
// X-Selenwright-External-Session-ID invariant.
func (h *Handler) proxySideEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == sideroute.HistorySettingsExact || strings.HasPrefix(r.URL.Path, sideroute.HistorySettingsPrefix) {
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

	prefix, ok := sideroute.PrefixFromContext(r.Context())
	var rest string
	if ok {
		rest = strings.TrimPrefix(r.URL.Path, prefix)
	} else {
		// Fallback when the handler is invoked without the mux middleware
		// (direct unit tests, or future callers that skip PrefixMiddleware).
		prefix, rest, ok = splitSidePath(r.URL.Path)
		if !ok {
			http.Error(w, "invalid side endpoint path", http.StatusBadRequest)
			return
		}
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

// hostInfo answers /host/<public-id> with the underlying backend endpoint
// metadata. Used by operator tooling to locate the physical node a session
// is running on.
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

// defaultBackend picks the first healthy pool — used by endpoints that do
// not have a session id to route on (e.g. /history/settings, artifact index
// listings).
func (h *Handler) defaultBackend() (config.BackendPool, error) {
	for _, pool := range h.pools {
		if h.health != nil && !h.health.Available(pool.ID) {
			continue
		}
		return pool, nil
	}
	return config.BackendPool{}, fmt.Errorf("no available backend")
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
