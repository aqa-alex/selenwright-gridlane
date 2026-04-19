package app

import (
	"net/http"

	"gridlane/internal/auth"
	"gridlane/internal/observe"
	"gridlane/internal/proxy"
	"gridlane/internal/sideroute"
)

// NewHandler exposes public, admin, catalog and WebDriver/Playwright proxy
// endpoints wired onto a shared mux, wrapped in the gridlane middleware
// chain. It is rebuilt by ReloadingHandler on every successful reload.
func NewHandler(opts Options, runtime Runtime) http.Handler {
	metrics := runtime.Metrics
	if metrics == nil {
		metrics = observe.NewMetrics()
	}
	mux := http.NewServeMux()
	webdriverProxy, proxyErr := proxy.NewHandler(proxy.Options{
		Config:                runtime.Catalog.Config(),
		Health:                runtime.Health,
		Credentials:           runtime.ProxyCredentials,
		SessionAttemptTimeout: opts.SessionAttemptTimeout,
		ProxyTimeout:          opts.ProxyTimeout,
		Metrics:               metrics,
	})
	mux.HandleFunc("/ping", getOnly(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"service": serviceName,
			"status":  "ok",
		})
	}))
	mux.HandleFunc("/status", getOnly(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, runtime.Health.PublicSnapshot())
	}))
	mux.HandleFunc("/config", getOnly(func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, runtime.Auth, auth.ScopeAdmin) {
			return
		}
		writeJSON(w, http.StatusOK, runtime.Catalog.SanitizedConfig(opts.ConfigPath))
	}))
	mux.HandleFunc("/quota", getOnly(func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, runtime.Auth, auth.ScopeUser) {
			return
		}
		writeJSON(w, http.StatusOK, runtime.Catalog.Quota())
	}))
	mux.HandleFunc("/metrics", getOnly(func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r, runtime.Auth, auth.ScopeAdmin) {
			return
		}
		metrics.Handler(runtime.Health).ServeHTTP(w, r)
	}))
	mux.Handle("/wd/hub/session", userScoped(runtime.Auth, unavailableOnError(webdriverProxy, proxyErr)))
	mux.Handle("/wd/hub/session/", userScoped(runtime.Auth, unavailableOnError(webdriverProxy, proxyErr)))
	mux.Handle("/session", userScoped(runtime.Auth, unavailableOnError(webdriverProxy, proxyErr)))
	mux.Handle("/session/", userScoped(runtime.Auth, unavailableOnError(webdriverProxy, proxyErr)))
	mux.Handle("/playwright/", userScoped(runtime.Auth, unavailableOnError(webdriverProxy, proxyErr)))
	mux.Handle("/host/", scoped(runtime.Auth, auth.ScopeSide, unavailableOnError(webdriverProxy, proxyErr)))
	// Each side prefix registration carries its matched prefix into the
	// request context so proxy.proxySideEndpoint does not re-scan the prefix
	// list on every call — see sideroute.PrefixMiddleware.
	for _, prefix := range sideroute.Prefixes {
		mux.Handle(prefix, scoped(runtime.Auth, auth.ScopeSide, sideroute.PrefixMiddleware(prefix, unavailableOnError(webdriverProxy, proxyErr))))
	}
	mux.Handle(sideroute.HistorySettingsExact, scoped(runtime.Auth, auth.ScopeSide, unavailableOnError(webdriverProxy, proxyErr)))
	mux.Handle(sideroute.HistorySettingsPrefix, scoped(runtime.Auth, auth.ScopeSide, unavailableOnError(webdriverProxy, proxyErr)))
	return buildMiddleware(metrics, mux)
}

// buildMiddleware applies the gridlane-wide middleware chain (metrics
// observation, security headers) to the final handler. Ordered outer-to-
// inner; additions go here so tests can reason about a single place.
func buildMiddleware(metrics *observe.Metrics, h http.Handler) http.Handler {
	chain := []func(http.Handler) http.Handler{
		metrics.Middleware,
		securityHeaders,
	}
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}
	return h
}
