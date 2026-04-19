package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"

	"gridlane/internal/config"
	"gridlane/internal/routing"
	"gridlane/internal/sessionid"
)

// proxyPlaywright accepts a WebSocket upgrade on /playwright/<browser>/<version>,
// picks a backend, mints a public session id and hands off to selenwright via
// the shared reverseProxy plumbing. The public id flows upstream through
// X-Selenwright-External-Session-ID so side endpoints can later resolve
// against the same key the upstream stored, and back to the client through
// X-Selenwright-Session-ID on the 101 response.
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

// newPlaywrightExternalSessionID returns a pw_<32-hex> token suitable for use
// as the selenwright-side session id. The pw_ prefix distinguishes Playwright
// sessions from WebDriver ones at every later path-routing decision.
func newPlaywrightExternalSessionID() (string, error) {
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("generate playwright external session id: %w", err)
	}
	return playwrightExternalSessionPrefix + hex.EncodeToString(randomBytes), nil
}
