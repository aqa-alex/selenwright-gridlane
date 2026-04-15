// Package auth owns Gridlane authentication policy and credential handling.
package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"

	"gridlane/internal/config"
)

const HeaderAdminToken = "X-Gridlane-Admin-Token"

type Scope string

const (
	ScopePublic Scope = "public"
	ScopeUser   Scope = "user"
	ScopeSide   Scope = "side"
	ScopeAdmin  Scope = "admin"
)

type SecretResolver interface {
	Resolve(ref string) (string, error)
}

type EnvFileResolver struct{}

func (EnvFileResolver) Resolve(ref string) (string, error) {
	if strings.HasPrefix(ref, "env:") {
		key := strings.TrimPrefix(ref, "env:")
		value, ok := os.LookupEnv(key)
		if !ok {
			return "", fmt.Errorf("secret env %q is not set", key)
		}
		if value == "" {
			return "", fmt.Errorf("secret env %q is empty", key)
		}
		return value, nil
	}
	if strings.HasPrefix(ref, "file:") {
		path := strings.TrimPrefix(ref, "file:")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		value := strings.TrimRight(string(data), "\r\n")
		if value == "" {
			return "", fmt.Errorf("secret file %q is empty", path)
		}
		return value, nil
	}
	return "", fmt.Errorf("unsupported secret reference %q", ref)
}

type Policy struct {
	users           map[string]string
	guestEnabled    bool
	adminToken      string
	adminConfigured bool
}

func NewPolicy(cfg config.Config, resolver SecretResolver) (*Policy, error) {
	policy := &Policy{
		users:        make(map[string]string, len(cfg.Users)),
		guestEnabled: cfg.Guest != nil,
	}
	for _, user := range cfg.Users {
		password, err := resolver.Resolve(user.PasswordRef)
		if err != nil {
			return nil, fmt.Errorf("resolve password for user %q: %w", user.Name, err)
		}
		policy.users[user.Name] = password
	}
	if cfg.Admin.TokenRef != "" {
		token, err := resolver.Resolve(cfg.Admin.TokenRef)
		if err != nil {
			return nil, fmt.Errorf("resolve admin token: %w", err)
		}
		policy.adminToken = token
		policy.adminConfigured = true
	}
	return policy, nil
}

func (p *Policy) AuthenticateBasic(username string, password string) bool {
	expected, ok := p.users[username]
	if !ok {
		return false
	}
	return constantTimeEqual(expected, password)
}

func (p *Policy) GuestEnabled() bool {
	return p.guestEnabled
}

func (p *Policy) AdminConfigured() bool {
	return p.adminConfigured
}

func (p *Policy) AuthenticateAdminToken(token string) bool {
	if !p.adminConfigured || token == "" {
		return false
	}
	return constantTimeEqual(p.adminToken, token)
}

func (p *Policy) Authorize(r *http.Request, scope Scope) Identity {
	switch scope {
	case ScopePublic:
		return Identity{Allowed: true, Scope: scope}
	case ScopeUser:
		username, password, ok := r.BasicAuth()
		if ok && p.AuthenticateBasic(username, password) {
			return Identity{Allowed: true, Scope: scope, Subject: username}
		}
		if p.guestEnabled {
			return Identity{Allowed: true, Scope: scope, Subject: "guest", Guest: true}
		}
		return Identity{Allowed: false, Scope: scope}
	case ScopeSide:
		username, password, ok := r.BasicAuth()
		if ok && p.AuthenticateBasic(username, password) {
			return Identity{Allowed: true, Scope: scope, Subject: username}
		}
		return Identity{Allowed: false, Scope: scope}
	case ScopeAdmin:
		if p.AuthenticateAdminToken(adminTokenFromRequest(r)) {
			return Identity{Allowed: true, Scope: scope, Subject: "admin", Admin: true}
		}
		return Identity{Allowed: false, Scope: scope}
	default:
		return Identity{Allowed: false, Scope: scope}
	}
}

func ScopeForPath(path string) Scope {
	if path == "/config" || path == "/metrics" {
		return ScopeAdmin
	}
	if isSideEndpoint(path) || strings.HasPrefix(path, "/host/") {
		return ScopeSide
	}
	if path == "/quota" {
		return ScopeUser
	}
	return ScopePublic
}

func isSideEndpoint(path string) bool {
	for _, prefix := range []string{
		"/vnc/",
		"/devtools/",
		"/video/",
		"/logs/",
		"/download/",
		"/downloads/",
		"/clipboard/",
		"/history/settings",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

type Identity struct {
	Allowed bool
	Scope   Scope
	Subject string
	Guest   bool
	Admin   bool
}

func adminTokenFromRequest(r *http.Request) string {
	if token := r.Header.Get(HeaderAdminToken); token != "" {
		return token
	}
	authorization := r.Header.Get("Authorization")
	if strings.HasPrefix(authorization, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	}
	return ""
}

func constantTimeEqual(expected string, actual string) bool {
	if len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
