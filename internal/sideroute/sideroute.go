// Package sideroute is the single source of truth for upstream side-endpoint
// paths routed through gridlane (VNC, devtools, video, logs, downloads,
// clipboard, artifact-history settings). Every consumer — mux wiring, auth
// scope resolution, proxy dispatch — must use these values, not its own
// private copy. Otherwise adding a new endpoint means touching three
// packages and the lists drift out of sync.
package sideroute

import "strings"

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
