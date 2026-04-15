package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"gridlane/internal/auth"
	"gridlane/internal/catalog"
	routerconfig "gridlane/internal/config"
	"gridlane/internal/health"
	"gridlane/internal/observe"
	"gridlane/internal/proxy"
)

const serviceName = "gridlane"

// Options is the Stage 2 process configuration parsed from CLI flags.
type Options struct {
	Listen                string
	ConfigPath            string
	GracefulPeriod        time.Duration
	SessionAttemptTimeout time.Duration
	ProxyTimeout          time.Duration
	ReloadOnSIGHUP        bool
	LogFormat             string
	MetricsListen         string
	Version               string
}

// ParseFlags parses Gridlane CLI flags without starting the service.
func ParseFlags(args []string) (Options, bool, error) {
	opts := Options{
		Listen:                ":4444",
		ConfigPath:            "router.json",
		GracefulPeriod:        15 * time.Second,
		SessionAttemptTimeout: 30 * time.Second,
		ProxyTimeout:          5 * time.Minute,
		ReloadOnSIGHUP:        true,
		LogFormat:             "text",
	}

	fs := flag.NewFlagSet("gridlane", flag.ContinueOnError)
	fs.StringVar(&opts.Listen, "listen", opts.Listen, "HTTP listen address")
	fs.StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "path to router.json")
	fs.DurationVar(&opts.GracefulPeriod, "graceful-period", opts.GracefulPeriod, "graceful shutdown period")
	fs.DurationVar(&opts.SessionAttemptTimeout, "session-attempt-timeout", opts.SessionAttemptTimeout, "upstream session creation timeout")
	fs.DurationVar(&opts.ProxyTimeout, "proxy-timeout", opts.ProxyTimeout, "upstream proxy timeout")
	fs.BoolVar(&opts.ReloadOnSIGHUP, "reload-on-sighup", opts.ReloadOnSIGHUP, "reload config on SIGHUP")
	fs.StringVar(&opts.LogFormat, "log-format", opts.LogFormat, "log format: text or json")
	fs.StringVar(&opts.MetricsListen, "metrics-listen", "", "optional metrics listen address")

	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return Options{}, false, err
	}
	if opts.LogFormat != "text" && opts.LogFormat != "json" {
		return Options{}, false, fmt.Errorf("unsupported log format %q", opts.LogFormat)
	}
	return opts, *showVersion, nil
}

type Runtime struct {
	Catalog          *catalog.Catalog
	Auth             *auth.Policy
	Health           *health.Manager
	ProxyCredentials proxy.CredentialStore
	Metrics          *observe.Metrics
}

// NewRuntime loads the strict router.json and resolves auth secrets.
func NewRuntime(opts Options) (Runtime, error) {
	cfg, err := routerconfig.LoadFile(opts.ConfigPath)
	if err != nil {
		return Runtime{}, fmt.Errorf("load config: %w", err)
	}
	cat, err := catalog.New(cfg)
	if err != nil {
		return Runtime{}, fmt.Errorf("build catalog: %w", err)
	}
	policy, err := auth.NewPolicy(cfg, auth.EnvFileResolver{})
	if err != nil {
		return Runtime{}, fmt.Errorf("build auth policy: %w", err)
	}
	backendCredentials, err := proxy.NewCredentialStore(cfg.BackendPools, auth.EnvFileResolver{})
	if err != nil {
		return Runtime{}, fmt.Errorf("build backend credential store: %w", err)
	}
	return Runtime{
		Catalog:          cat,
		Auth:             policy,
		Health:           health.NewManager(cfg.BackendPools),
		ProxyCredentials: backendCredentials,
		Metrics:          observe.NewMetrics(),
	}, nil
}

// Run starts the HTTP surface and WebDriver proxy.
func Run(ctx context.Context, opts Options, logger *slog.Logger) error {
	handler, err := NewReloadingHandler(opts)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              opts.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       opts.ProxyTimeout,
		WriteTimeout:      opts.ProxyTimeout,
		IdleTimeout:       2 * opts.GracefulPeriod,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 2)
	startServer := func(name string, srv *http.Server) {
		go func() {
			logger.Info("starting "+name, "listen", srv.Addr)
			errCh <- srv.ListenAndServe()
		}()
	}
	var metricsSrv *http.Server
	if opts.MetricsListen != "" {
		metricsSrv = &http.Server{
			Addr:              opts.MetricsListen,
			Handler:           handler.MetricsHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       opts.GracefulPeriod,
			MaxHeaderBytes:    1 << 20,
		}
	}

	reloadCtx, stopReload := context.WithCancel(ctx)
	defer stopReload()
	if opts.ReloadOnSIGHUP {
		hupCh := make(chan os.Signal, 1)
		signal.Notify(hupCh, syscall.SIGHUP)
		defer signal.Stop(hupCh)
		go func() {
			for {
				select {
				case <-reloadCtx.Done():
					return
				case <-hupCh:
					if err := handler.Reload(); err != nil {
						logger.Error("config reload failed", "path", opts.ConfigPath, "error", err)
						continue
					}
					logger.Info("config reloaded", "path", opts.ConfigPath)
				}
			}
		}()
	}

	startServer("gridlane", srv)
	if metricsSrv != nil {
		startServer("gridlane metrics", metricsSrv)
	}

	select {
	case <-ctx.Done():
		return shutdownServers(opts.GracefulPeriod, srv, metricsSrv)
	case err := <-errCh:
		stopReload()
		_ = shutdownServers(opts.GracefulPeriod, srv, metricsSrv)
		return err
	}
}

type ReloadingHandler struct {
	opts          Options
	current       atomic.Value
	currentHealth atomic.Value
	metrics       *observe.Metrics
}

func NewReloadingHandler(opts Options) (*ReloadingHandler, error) {
	handler := &ReloadingHandler{opts: opts, metrics: observe.NewMetrics()}
	if err := handler.Reload(); err != nil {
		return nil, err
	}
	return handler, nil
}

func (h *ReloadingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	current := h.current.Load()
	if current == nil {
		http.Error(w, "runtime is not loaded", http.StatusServiceUnavailable)
		return
	}
	current.(http.Handler).ServeHTTP(w, r)
}

func (h *ReloadingHandler) Reload() error {
	runtime, err := NewRuntime(h.opts)
	if err != nil {
		return err
	}
	runtime.Metrics = h.metrics
	h.current.Store(NewHandler(h.opts, runtime))
	h.currentHealth.Store(runtime.Health)
	return nil
}

func (h *ReloadingHandler) Snapshot() health.Snapshot {
	current := h.currentHealth.Load()
	if current == nil {
		return health.Snapshot{Service: serviceName, Status: "degraded"}
	}
	return current.(*health.Manager).Snapshot()
}

func (h *ReloadingHandler) MetricsHandler() http.Handler {
	return h.metrics.Handler(h)
}

func shutdownServers(timeout time.Duration, servers ...*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var shutdownErr error
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		if err := srv.Shutdown(ctx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	return shutdownErr
}

// NewHandler exposes public, admin, catalog, and WebDriver proxy endpoints.
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
		writeJSON(w, http.StatusOK, runtime.Health.Snapshot())
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
	for _, prefix := range []string{
		"/vnc/",
		"/devtools/",
		"/video/",
		"/logs/",
		"/download/",
		"/downloads/",
		"/clipboard/",
	} {
		mux.Handle(prefix, scoped(runtime.Auth, auth.ScopeSide, unavailableOnError(webdriverProxy, proxyErr)))
	}
	mux.Handle("/history/settings", scoped(runtime.Auth, auth.ScopeSide, unavailableOnError(webdriverProxy, proxyErr)))
	mux.Handle("/history/settings/", scoped(runtime.Auth, auth.ScopeSide, unavailableOnError(webdriverProxy, proxyErr)))
	return metrics.Middleware(securityHeaders(mux))
}

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
	_ = json.NewEncoder(w).Encode(payload)
}
