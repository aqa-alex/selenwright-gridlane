package sideroute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsSide(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"/vnc/abc", true},
		{"/devtools/abc/page", true},
		{"/video/abc.mp4", true},
		{"/logs/abc", true},
		{"/download/abc/file.txt", true},
		{"/downloads/abc/file.txt", true},
		{"/clipboard/abc", true},
		{"/history/settings", true},
		{"/history/settings/", true},
		{"/history/settings/audit", true},
		{"/history", false},
		{"/history-settings", false},
		{"/vnc", false},
		{"/ping", false},
		{"/wd/hub/session", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsSide(tc.path); got != tc.want {
			t.Errorf("IsSide(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestPrefixMiddlewareStampsContext(t *testing.T) {
	t.Parallel()
	var seen string
	var hasPrefix bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, hasPrefix = PrefixFromContext(r.Context())
	})

	handler := PrefixMiddleware("/video/", next)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/video/abc.mp4", nil))

	if !hasPrefix || seen != "/video/" {
		t.Fatalf("PrefixFromContext = (%q, %v), want (/video/, true)", seen, hasPrefix)
	}
}

func TestPrefixFromContextReturnsFalseWhenAbsent(t *testing.T) {
	t.Parallel()
	if prefix, ok := PrefixFromContext(context.Background()); ok || prefix != "" {
		t.Fatalf("PrefixFromContext(empty ctx) = (%q, %v), want (\"\", false)", prefix, ok)
	}
}

func TestMatchPrefix(t *testing.T) {
	t.Parallel()
	prefix, rest, ok := MatchPrefix("/video/abc.mp4")
	if !ok || prefix != "/video/" || rest != "abc.mp4" {
		t.Fatalf("MatchPrefix(/video/abc.mp4) = (%q, %q, %v), want (/video/, abc.mp4, true)", prefix, rest, ok)
	}

	if _, _, ok := MatchPrefix("/history/settings"); ok {
		t.Fatal("MatchPrefix(/history/settings) must not match — history is handled separately by callers")
	}
	if _, _, ok := MatchPrefix("/something-else"); ok {
		t.Fatal("MatchPrefix(/something-else) ok = true, want false")
	}
}
