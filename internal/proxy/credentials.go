package proxy

import (
	"fmt"
	"net/http"

	"gridlane/internal/config"
)

// BackendCredential holds the BasicAuth pair gridlane presents to a backend
// pool for outgoing requests. Kept in memory after resolution so upstream
// requests are cheap.
type BackendCredential struct {
	Username string
	Password string
}

type CredentialStore map[string]BackendCredential

// NewCredentialStore resolves per-backend BasicAuth secrets against the given
// resolver at startup. Pools without a `credentials` block are skipped
// silently; a non-nil resolver is required the moment any pool needs one.
func NewCredentialStore(pools []config.BackendPool, resolver SecretResolver) (CredentialStore, error) {
	store := CredentialStore{}
	for _, pool := range pools {
		if pool.Credentials == nil {
			continue
		}
		if resolver == nil {
			return nil, fmt.Errorf("backend credentials configured for %q but no resolver was provided", pool.ID)
		}
		username, err := resolver.Resolve(pool.Credentials.UsernameRef)
		if err != nil {
			return nil, fmt.Errorf("resolve backend username for %q: %w", pool.ID, err)
		}
		password, err := resolver.Resolve(pool.Credentials.PasswordRef)
		if err != nil {
			return nil, fmt.Errorf("resolve backend password for %q: %w", pool.ID, err)
		}
		store[pool.ID] = BackendCredential{Username: username, Password: password}
	}
	return store, nil
}

func (h *Handler) applyBackendCredentials(req *http.Request, backendID string) {
	credential, ok := h.credentials[backendID]
	if !ok {
		return
	}
	req.SetBasicAuth(credential.Username, credential.Password)
}
