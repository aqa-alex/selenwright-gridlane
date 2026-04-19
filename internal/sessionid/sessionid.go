// Package sessionid owns public session id encoding and decoding.
package sessionid

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const Prefix = "r1"

// TokenForBackend derives a stable, opaque route token from a backend id.
//
// The HMAC key below is a domain separator, not a secret: it hides backend
// ids from clients and makes tokens look random, but it is not signed or
// verified. Two independent gridlane deployments sharing backend ids will
// produce colliding tokens. If federated / per-deployment isolation becomes
// a requirement (technical debt, tracked for a future release), accept a
// secret key via `-route-salt` with the existing env:/file: secret-ref
// plumbing and swap it in here.
func TokenForBackend(backendID string) (string, error) {
	if backendID == "" {
		return "", fmt.Errorf("backend id is required")
	}
	sum := hmac.New(sha256.New, []byte("gridlane-route-token-v1"))
	_, _ = sum.Write([]byte(backendID))
	return hex.EncodeToString(sum.Sum(nil)[:16]), nil
}

func Encode(routeToken string, upstreamSessionID string) (string, error) {
	if routeToken == "" {
		return "", fmt.Errorf("route token is required")
	}
	if upstreamSessionID == "" {
		return "", fmt.Errorf("upstream session id is required")
	}
	if strings.Contains(routeToken, "_") {
		return "", fmt.Errorf("route token must not contain underscores")
	}
	return Prefix + "_" + routeToken + "_" + upstreamSessionID, nil
}

func Decode(publicSessionID string) (Parts, error) {
	prefix, rest, ok := strings.Cut(publicSessionID, "_")
	if !ok || prefix != Prefix {
		return Parts{}, fmt.Errorf("session id must start with %s_", Prefix)
	}
	routeToken, upstreamSessionID, ok := strings.Cut(rest, "_")
	if !ok || routeToken == "" || upstreamSessionID == "" {
		return Parts{}, fmt.Errorf("session id must contain route token and upstream session id")
	}
	return Parts{
		RouteToken:        routeToken,
		UpstreamSessionID: upstreamSessionID,
	}, nil
}

type Parts struct {
	RouteToken        string
	UpstreamSessionID string
}
