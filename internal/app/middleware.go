package app

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"gridlane/internal/auth"
)

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func userScoped(policy *auth.Policy, next http.Handler) http.Handler {
	return scoped(policy, auth.ScopeUser, next)
}

func scoped(policy *auth.Policy, scope auth.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, policy, scope) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unavailableOnError(next http.Handler, err error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authorize(w http.ResponseWriter, r *http.Request, policy *auth.Policy, scope auth.Scope) bool {
	if policy == nil {
		http.Error(w, "auth policy is not configured", http.StatusServiceUnavailable)
		return false
	}
	identity := policy.Authorize(r, scope)
	if identity.Allowed {
		return true
	}
	if scope == auth.ScopeUser || scope == auth.ScopeSide {
		w.Header().Set("WWW-Authenticate", `Basic realm="gridlane"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	http.Error(w, "admin token required", http.StatusUnauthorized)
	return false
}

func getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Response headers are already committed — log so an operator can see
		// broken pipe / write timeouts instead of silently truncated JSON.
		slog.Warn("write JSON response failed", "err", err)
	}
}
