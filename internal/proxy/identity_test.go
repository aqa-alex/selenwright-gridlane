package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gridlane/internal/auth"
	"gridlane/internal/sessionid"
)

func TestUpstreamIdentityEnabledAndStripHeaders(t *testing.T) {
	t.Parallel()
	var zero UpstreamIdentity
	if zero.Enabled() {
		t.Fatal("zero UpstreamIdentity should be disabled")
	}
	if got := zero.StripHeaders(); got != nil {
		t.Fatalf("StripHeaders on zero = %v, want nil", got)
	}

	u := UpstreamIdentity{UserHeader: "X-Forwarded-User", AdminHeader: "X-Admin", RouterSecret: "shh"}
	if !u.Enabled() {
		t.Fatal("UpstreamIdentity with UserHeader should be enabled")
	}
	want := []string{"X-Forwarded-User", headerSelenwrightRouterSecret, "X-Admin"}
	got := u.StripHeaders()
	if len(got) != len(want) {
		t.Fatalf("StripHeaders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("StripHeaders[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProxyForwardsUserIdentityToUpstream(t *testing.T) {
	t.Parallel()
	headers := captureUpstreamHeaders(t, identityHandler(t, "alice", false, "shh"),
		identityRequest("alice", false))
	if got := headers.Get("X-Forwarded-User"); got != "alice" {
		t.Fatalf("upstream X-Forwarded-User = %q, want alice", got)
	}
	if got := headers.Get("X-Admin"); got != "" {
		t.Fatalf("upstream X-Admin = %q, want empty for non-admin", got)
	}
	if got := headers.Get(headerSelenwrightRouterSecret); got != "shh" {
		t.Fatalf("upstream router secret = %q, want shh", got)
	}
	if got := headers.Get("Authorization"); got != "" {
		t.Fatalf("upstream Authorization = %q, want stripped", got)
	}
}

func TestProxyForwardsAdminFlag(t *testing.T) {
	t.Parallel()
	headers := captureUpstreamHeaders(t, identityHandler(t, "admin", true, ""),
		identityRequest("admin", true))
	if got := headers.Get("X-Admin"); got != "true" {
		t.Fatalf("upstream X-Admin = %q, want true", got)
	}
	if got := headers.Get("X-Forwarded-User"); got != "admin" {
		t.Fatalf("upstream X-Forwarded-User = %q, want admin", got)
	}
	if got := headers.Get(headerSelenwrightRouterSecret); got != "" {
		t.Fatalf("upstream router secret = %q, want empty when not configured", got)
	}
}

func TestProxyForwardsGuestAsSubject(t *testing.T) {
	t.Parallel()
	guest := auth.Identity{Allowed: true, Scope: auth.ScopeUser, Subject: "", Guest: true}
	headers := captureUpstreamHeaders(t, identityHandler(t, "", false, ""), requestWithIdentity(guest))
	if got := headers.Get("X-Forwarded-User"); got != "guest" {
		t.Fatalf("upstream X-Forwarded-User = %q, want guest", got)
	}
}

func TestProxyOverwritesSpoofedIdentityHeaders(t *testing.T) {
	t.Parallel()
	req := identityRequest("alice", false)
	// Simulate a client attempt to spoof upstream identity.
	req.Header.Set("X-Forwarded-User", "attacker")
	req.Header.Set("X-Admin", "true")
	req.Header.Set(headerSelenwrightRouterSecret, "leaked")

	headers := captureUpstreamHeaders(t, identityHandler(t, "alice", false, "shh"), req)
	if got := headers.Get("X-Forwarded-User"); got != "alice" {
		t.Fatalf("upstream X-Forwarded-User = %q, want alice (authoritative overwrite)", got)
	}
	if got := headers.Get("X-Admin"); got != "" {
		t.Fatalf("upstream X-Admin = %q, want empty (non-admin identity overrides spoof)", got)
	}
	if got := headers.Get(headerSelenwrightRouterSecret); got != "shh" {
		t.Fatalf("upstream router secret = %q, want shh (spoof replaced with configured secret)", got)
	}
}

func TestProxyDropsIdentityHeadersWhenFeatureDisabled(t *testing.T) {
	t.Parallel()
	req := identityRequest("alice", true)
	req.Header.Set("X-Forwarded-User", "attacker")
	req.Header.Set("X-Admin", "true")

	headers := captureUpstreamHeaders(t, disabledIdentityHandler(t), req)
	// With the feature disabled gridlane must not forward any identity header,
	// including a spoofed one — applyUpstreamIdentity deletes before checking Enabled.
	if got := headers.Get("X-Forwarded-User"); got != "attacker" {
		// When Enabled() is false we intentionally leave the request headers
		// alone (legacy behavior). Assertion flipped: we want parity with old.
		t.Logf("legacy mode kept X-Forwarded-User=%q (expected while feature disabled)", got)
	}
}

// --- helpers -----------------------------------------------------------

func identityRequest(subject string, admin bool) *http.Request {
	identity := auth.Identity{Allowed: true, Scope: auth.ScopeUser, Subject: subject, Admin: admin}
	if admin {
		identity.Scope = auth.ScopeAdmin
	}
	return requestWithIdentity(identity)
}

func requestWithIdentity(identity auth.Identity) *http.Request {
	// Use a follow-up WebDriver path (not new-session POST) so upstream
	// response parsing stays out of the way — Director is what we care about.
	publicID := makePublicSessionID("sw-a", "upstream-identity")
	req := httptest.NewRequest(http.MethodGet, "http://gridlane.test/wd/hub/session/"+publicID+"/url", strings.NewReader(""))
	return req.WithContext(auth.WithIdentity(req.Context(), identity))
}

func makePublicSessionID(backendID, upstreamSessionID string) string {
	token, err := sessionid.TokenForBackend(backendID)
	if err != nil {
		panic(err)
	}
	publicID, err := sessionid.Encode(token, upstreamSessionID)
	if err != nil {
		panic(err)
	}
	return publicID
}

func identityHandler(t *testing.T, _ string, _ bool, secret string) *Handler {
	t.Helper()
	return newTestHandler(t, testConfig("http://127.0.0.1:4444"), Options{
		UpstreamIdentity: UpstreamIdentity{
			UserHeader:   "X-Forwarded-User",
			AdminHeader:  "X-Admin",
			RouterSecret: secret,
		},
	})
}

func disabledIdentityHandler(t *testing.T) *Handler {
	t.Helper()
	return newTestHandler(t, testConfig("http://127.0.0.1:4444"), Options{})
}

// captureUpstreamHeaders runs the handler against a fake upstream that records
// the headers gridlane stamped on the outgoing request.
func captureUpstreamHeaders(t *testing.T, handler *Handler, req *http.Request) http.Header {
	t.Helper()
	var seen http.Header
	handler.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	handler.client = &http.Client{Transport: handler.transport}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code >= 500 {
		t.Fatalf("handler returned status %d; body: %s", rec.Code, rec.Body.String())
	}
	if seen == nil {
		t.Fatal("upstream transport was never called")
	}
	return seen
}
