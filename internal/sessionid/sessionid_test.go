package sessionid

import "testing"

func TestTokenForBackendIsStableAndOpaque(t *testing.T) {
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
	if _, err := Decode("abcdef0123456789"); err == nil {
		t.Fatal("Decode() error = nil, want error")
	}
}
