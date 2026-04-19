package sessionid

import (
	"strings"
	"testing"
)

func TestTokenForBackendIsStableAndOpaque(t *testing.T) {
	t.Parallel()
	first, err := TokenForBackend("sw-local")
	if err != nil {
		t.Fatalf("TokenForBackend() error = %v", err)
	}
	second, err := TokenForBackend("sw-local")
	if err != nil {
		t.Fatalf("TokenForBackend() error = %v", err)
	}
	if first != second {
		t.Fatalf("token changed: %q != %q", first, second)
	}
	if first == "sw-local" {
		t.Fatalf("token = backend id %q, want opaque token", first)
	}
}

func TestEncodeDecode(t *testing.T) {
	t.Parallel()
	token, err := TokenForBackend("sw-local")
	if err != nil {
		t.Fatalf("TokenForBackend() error = %v", err)
	}
	publicID, err := Encode(token, "upstream-123")
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	parts, err := Decode(publicID)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if parts.RouteToken != token {
		t.Fatalf("RouteToken = %q, want %q", parts.RouteToken, token)
	}
	if parts.UpstreamSessionID != "upstream-123" {
		t.Fatalf("UpstreamSessionID = %q, want upstream-123", parts.UpstreamSessionID)
	}
}

func TestDecodeRejectsLegacyID(t *testing.T) {
	t.Parallel()
	if _, err := Decode("abcdef0123456789"); err == nil {
		t.Fatal("Decode() error = nil, want error")
	}
}

func TestDecodePreservesMultiUnderscoreUpstreamID(t *testing.T) {
	t.Parallel()
	token, err := TokenForBackend("sw-local")
	if err != nil {
		t.Fatalf("TokenForBackend() error = %v", err)
	}
	// selenwright may return an upstream id with underscores — strings.Cut
	// must split only on the FIRST separator so the rest stays intact.
	const upstream = "pw_abcdef0123_456789"
	publicID, err := Encode(token, upstream)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	parts, err := Decode(publicID)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if parts.UpstreamSessionID != upstream {
		t.Fatalf("UpstreamSessionID = %q, want %q (underscores in upstream id must survive a round trip)", parts.UpstreamSessionID, upstream)
	}
}

func TestDecodeEdgeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  string
	}{
		{"", "must start with r1_"},
		{"r1_", "must contain route token and upstream session id"},
		{"r2_token_upstream", "must start with r1_"},
		{"r1_onlyToken", "must contain route token and upstream session id"},
		{"r1_token_", "must contain route token and upstream session id"},
		{"r1__upstream", "must contain route token and upstream session id"},
	}
	for _, tc := range cases {
		_, err := Decode(tc.input)
		if err == nil {
			t.Errorf("Decode(%q) error = nil, want %q", tc.input, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Decode(%q) error = %v, want substring %q", tc.input, err, tc.want)
		}
	}
}

func TestEncodeRejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := Encode("", "upstream"); err == nil {
		t.Fatal("Encode(empty token) error = nil, want error")
	}
	if _, err := Encode("token", ""); err == nil {
		t.Fatal("Encode(empty upstream) error = nil, want error")
	}
	if _, err := Encode("bad_token", "upstream"); err == nil {
		t.Fatal("Encode(token with underscore) error = nil, want error")
	}
}
