// Package sideroute is the single source of truth for upstream side-endpoint
// paths routed through gridlane (VNC, devtools, video, logs, downloads,
// clipboard, artifact-history settings). Every consumer — mux wiring, auth
// scope resolution, proxy dispatch — must use these values, not its own
// private copy. Otherwise adding a new endpoint means touching three
// packages and the lists drift out of sync.
package sideroute

import (
	"context"
	"net/http"
	"strings"
)

// Prefixes is the exhaustive list of path prefixes that funnel to upstream
// side endpoints. Each element ends with "/" so strings.HasPrefix matches
// session-addressed paths cleanly.
var Prefixes = []string{
	"/vnc/",
	"/devtools/",
	"/video/",
	"/logs/",
	"/download/",
	"/downloads/",
	"/clipboard/",
}

// HistorySettingsExact is the exact path for artifact history settings
// (the only side endpoint that does not take a session id).
const HistorySettingsExact = "/history/settings"

// HistorySettingsPrefix covers nested subpaths of the artifact history API.
const HistorySettingsPrefix = "/history/settings/"

// IsSide reports whether the path belongs to an upstream side endpoint.
func IsSide(path string) bool {
	if path == HistorySettingsExact || strings.HasPrefix(path, HistorySettingsPrefix) {
		return true
	}
	for _, prefix := range Prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// MatchPrefix returns the Prefixes element that starts the given path, plus
// the remainder after the prefix. It matches only the session-addressed
// prefixes (not HistorySettingsExact / HistorySettingsPrefix, which are
// handled specially by callers).
func MatchPrefix(path string) (prefix string, rest string, ok bool) {
	for _, p := range Prefixes {
		if strings.HasPrefix(path, p) {
			return p, strings.TrimPrefix(path, p), true
		}
	}
	return "", "", false
}

// contextKey isolates our context value from any other key type in the
// program — it is an unexported zero-size struct as per
// https://pkg.go.dev/context#Context recommendations.
type contextKey struct{}

var prefixKey = contextKey{}

// WithPrefix stamps the matched side prefix onto the request context so a
// downstream handler can look it up in O(1) instead of re-scanning Prefixes.
func WithPrefix(ctx context.Context, prefix string) context.Context {
	return context.WithValue(ctx, prefixKey, prefix)
}

// PrefixFromContext retrieves the prefix that was stamped by PrefixMiddleware
// (or any caller that used WithPrefix). Returns (prefix, true) when present.
func PrefixFromContext(ctx context.Context) (string, bool) {
	prefix, ok := ctx.Value(prefixKey).(string)
	return prefix, ok && prefix != ""
}

// PrefixMiddleware wraps a handler so the incoming request carries the matched
// side prefix in its context. mux wiring calls this once per registered
// prefix, which means proxySideEndpoint never needs to re-derive the prefix
// from the URL.
func PrefixMiddleware(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithPrefix(r.Context(), prefix)))
	})
}
