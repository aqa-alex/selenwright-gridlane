package sideroute

import "testing"

func TestIsSide(t *testing.T) {
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

func TestMatchPrefix(t *testing.T) {
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
