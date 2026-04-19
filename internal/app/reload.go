package app

import (
	"net/http"
	"sync/atomic"

	"gridlane/internal/health"
	"gridlane/internal/observe"
)

// ReloadingHandler wraps the active request handler in an atomic.Value so
// SIGHUP can swap in a new Runtime without blocking in-flight requests.
// Reload is fail-closed: a bad config leaves the previous runtime live.
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
	handler, ok := h.current.Load().(http.Handler)
	if !ok || handler == nil {
		http.Error(w, "runtime is not loaded", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}

// Reload loads router.json from disk and atomically replaces the active
// handler and health manager. On error the previous runtime stays active.
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

// Snapshot returns the health snapshot of the currently-active runtime.
// Degraded status is reported if no runtime has been loaded yet.
func (h *ReloadingHandler) Snapshot() health.Snapshot {
	mgr, ok := h.currentHealth.Load().(*health.Manager)
	if !ok || mgr == nil {
		return health.Snapshot{Service: serviceName, Status: "degraded"}
	}
	return mgr.Snapshot()
}

// MetricsHandler returns the Prometheus handler backed by the shared metrics
// instance. Suitable for attaching to an optional separate -metrics-listen.
func (h *ReloadingHandler) MetricsHandler() http.Handler {
	return h.metrics.Handler(h)
}
