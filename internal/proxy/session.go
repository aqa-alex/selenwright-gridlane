package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gridlane/internal/config"
	"gridlane/internal/routing"
	"gridlane/internal/sessionid"
)

// createSession handles the WebDriver new-session POST. It validates the
// client payload, picks a backend via Selector.SelectFirst, relays the
// create to the upstream, classifies the status for health/metrics, and
// rewrites the upstream session id into the public r1_<token>_<id> form
// before streaming the response back.
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

	ctx, cancel := context.WithTimeout(r.Context(), h.sessionAttemptTimeout)
	defer cancel()
	upstreamRequest, err := h.newUpstreamRequest(ctx, r, backend, r.URL.Path, bytes.NewReader(upstreamBody))
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

// proxyWebDriverSession handles follow-up /session/{id}/... calls by decoding
// the public session id back to backend + upstream id and streaming through
// reverseProxy.
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

// routeSession decodes the client-facing public id and maps its route token
// to a configured backend. Unknown tokens are 404, malformed ids are 400 —
// both surfaced via routeError so callers can write a consistent status.
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
