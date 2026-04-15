// Package observe owns logging, metrics, and tracing integration.
package observe

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gridlane/internal/health"
)

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type Metrics struct {
	mu                sync.Mutex
	httpRequests      map[httpKey]uint64
	httpDurations     map[httpDurationKey]*histogram
	proxyRequests     map[proxyKey]uint64
	proxyDurations    map[proxyDurationKey]*histogram
	websocketSessions map[websocketKey]uint64
}

type httpKey struct {
	Method string
	Route  string
	Status string
}

type httpDurationKey struct {
	Method string
	Route  string
}

type proxyKey struct {
	Protocol  string
	BackendID string
	Outcome   string
}

type proxyDurationKey struct {
	Protocol  string
	BackendID string
}

type websocketKey struct {
	BackendID string
	Event     string
}

type histogram struct {
	Buckets []uint64
	Count   uint64
	Sum     float64
}

type HealthSource interface {
	Snapshot() health.Snapshot
}

func NewMetrics() *Metrics {
	return &Metrics{
		httpRequests:      map[httpKey]uint64{},
		httpDurations:     map[httpDurationKey]*histogram{},
		proxyRequests:     map[proxyKey]uint64{},
		proxyDurations:    map[proxyDurationKey]*histogram{},
		websocketSessions: map[websocketKey]uint64{},
	}
}

func (m *Metrics) Middleware(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(rec, r)
		m.RecordHTTPRequest(r.Method, r.URL.Path, rec.status, time.Since(started))
	})
}

func (m *Metrics) RecordHTTPRequest(method string, path string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	route := RouteLabel(path)
	statusText := strconv.Itoa(status)
	seconds := duration.Seconds()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.httpRequests[httpKey{Method: method, Route: route, Status: statusText}]++
	m.histogramFor(m.httpDurations, httpDurationKey{Method: method, Route: route}).observe(seconds)
}

func (m *Metrics) RecordProxyRequest(protocol string, backendID string, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	seconds := duration.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proxyRequests[proxyKey{Protocol: protocol, BackendID: backendID, Outcome: outcome}]++
	m.proxyHistogramFor(m.proxyDurations, proxyDurationKey{Protocol: protocol, BackendID: backendID}).observe(seconds)
}

func (m *Metrics) RecordWebSocketSession(backendID string, event string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.websocketSessions[websocketKey{BackendID: backendID, Event: event}]++
}

func (m *Metrics) Handler(healthSource HealthSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if m == nil {
			return
		}
		m.WritePrometheus(w, healthSource)
	})
}

func (m *Metrics) WritePrometheus(w io.Writer, healthSource HealthSource) {
	m.mu.Lock()
	httpRequests := sortedHTTPRequests(m.httpRequests)
	httpDurations := sortedHTTPDurations(m.httpDurations)
	proxyRequests := sortedProxyRequests(m.proxyRequests)
	proxyDurations := sortedProxyDurations(m.proxyDurations)
	websocketSessions := sortedWebSocketSessions(m.websocketSessions)
	m.mu.Unlock()

	writeHelp(w, "gridlane_http_requests_total", "Total HTTP requests handled by Gridlane.")
	writeType(w, "gridlane_http_requests_total", "counter")
	for _, item := range httpRequests {
		writef(w, "gridlane_http_requests_total{method=%q,route=%q,status=%q} %d\n", item.Key.Method, item.Key.Route, item.Key.Status, item.Value)
	}

	writeHelp(w, "gridlane_http_request_duration_seconds", "HTTP request duration in seconds.")
	writeType(w, "gridlane_http_request_duration_seconds", "histogram")
	for _, item := range httpDurations {
		writeHistogram(w, "gridlane_http_request_duration_seconds", fmt.Sprintf("method=%q,route=%q", item.Key.Method, item.Key.Route), item.Value)
	}

	writeHelp(w, "gridlane_proxy_requests_total", "Total upstream proxy requests by protocol, backend, and outcome.")
	writeType(w, "gridlane_proxy_requests_total", "counter")
	for _, item := range proxyRequests {
		writef(w, "gridlane_proxy_requests_total{protocol=%q,backend=%q,outcome=%q} %d\n", item.Key.Protocol, item.Key.BackendID, item.Key.Outcome, item.Value)
	}

	writeHelp(w, "gridlane_proxy_request_duration_seconds", "Upstream proxy request duration in seconds.")
	writeType(w, "gridlane_proxy_request_duration_seconds", "histogram")
	for _, item := range proxyDurations {
		writeHistogram(w, "gridlane_proxy_request_duration_seconds", fmt.Sprintf("protocol=%q,backend=%q", item.Key.Protocol, item.Key.BackendID), item.Value)
	}

	writeHelp(w, "gridlane_websocket_sessions_total", "Total Playwright WebSocket session events.")
	writeType(w, "gridlane_websocket_sessions_total", "counter")
	for _, item := range websocketSessions {
		writef(w, "gridlane_websocket_sessions_total{backend=%q,event=%q} %d\n", item.Key.BackendID, item.Key.Event, item.Value)
	}

	if healthSource == nil {
		return
	}
	snapshot := healthSource.Snapshot()
	writeHelp(w, "gridlane_backend_available", "Backend availability from Gridlane health state.")
	writeType(w, "gridlane_backend_available", "gauge")
	writeHelp(w, "gridlane_backend_failures_total", "Backend failure count from Gridlane health state.")
	writeType(w, "gridlane_backend_failures_total", "gauge")
	for _, backend := range snapshot.Backends {
		available := 0
		if backend.Available {
			available = 1
		}
		protocols := make([]string, 0, len(backend.Protocols))
		for _, protocol := range backend.Protocols {
			protocols = append(protocols, string(protocol))
		}
		sort.Strings(protocols)
		labels := fmt.Sprintf("backend=%q,region=%q,protocols=%q", backend.ID, backend.Region, strings.Join(protocols, ","))
		writef(w, "gridlane_backend_available{%s} %d\n", labels, available)
		writef(w, "gridlane_backend_failures_total{%s} %d\n", labels, backend.Failures)
	}
}

func RouteLabel(path string) string {
	switch {
	case path == "":
		return "/"
	case path == "/ping", path == "/status", path == "/config", path == "/quota", path == "/metrics", path == "/history/settings":
		return path
	case path == "/wd/hub/session" || strings.HasPrefix(path, "/wd/hub/session/"):
		return "/wd/hub/session/:session"
	case path == "/session" || strings.HasPrefix(path, "/session/"):
		return "/session/:session"
	case strings.HasPrefix(path, "/playwright/"):
		return "/playwright/:browser/:version"
	case strings.HasPrefix(path, "/host/"):
		return "/host/:session"
	case strings.HasPrefix(path, "/vnc/"):
		return "/vnc/:session"
	case strings.HasPrefix(path, "/devtools/"):
		return "/devtools/:session"
	case strings.HasPrefix(path, "/video/"):
		return "/video/:session"
	case strings.HasPrefix(path, "/logs/"):
		return "/logs/:session"
	case strings.HasPrefix(path, "/download/"):
		return "/download/:session"
	case strings.HasPrefix(path, "/downloads/"):
		return "/downloads/:session"
	case strings.HasPrefix(path, "/clipboard/"):
		return "/clipboard/:session"
	default:
		return "other"
	}
}

func (m *Metrics) histogramFor(values map[httpDurationKey]*histogram, key httpDurationKey) *histogram {
	h, ok := values[key]
	if !ok {
		h = newHistogram()
		values[key] = h
	}
	return h
}

func (m *Metrics) proxyHistogramFor(values map[proxyDurationKey]*histogram, key proxyDurationKey) *histogram {
	h, ok := values[key]
	if !ok {
		h = newHistogram()
		values[key] = h
	}
	return h
}

func newHistogram() *histogram {
	return &histogram{Buckets: make([]uint64, len(latencyBuckets))}
}

func (h *histogram) observe(value float64) {
	h.Count++
	h.Sum += value
	for i, bucket := range latencyBuckets {
		if value <= bucket {
			h.Buckets[i]++
		}
	}
}

func writeHistogram(w io.Writer, name string, labels string, h *histogram) {
	for i, bucket := range latencyBuckets {
		writef(w, "%s_bucket{%s,le=%q} %d\n", name, labels, formatBucket(bucket), h.Buckets[i])
	}
	writef(w, "%s_bucket{%s,le=%q} %d\n", name, labels, "+Inf", h.Count)
	writef(w, "%s_sum{%s} %g\n", name, labels, h.Sum)
	writef(w, "%s_count{%s} %d\n", name, labels, h.Count)
}

func writeHelp(w io.Writer, name string, description string) {
	writef(w, "# HELP %s %s\n", name, description)
}

func writeType(w io.Writer, name string, metricType string) {
	writef(w, "# TYPE %s %s\n", name, metricType)
}

func writef(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func formatBucket(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func sortedHTTPRequests(values map[httpKey]uint64) []struct {
	Key   httpKey
	Value uint64
} {
	items := make([]struct {
		Key   httpKey
		Value uint64
	}, 0, len(values))
	for key, value := range values {
		items = append(items, struct {
			Key   httpKey
			Value uint64
		}{Key: key, Value: value})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Key.Route != items[j].Key.Route {
			return items[i].Key.Route < items[j].Key.Route
		}
		if items[i].Key.Method != items[j].Key.Method {
			return items[i].Key.Method < items[j].Key.Method
		}
		return items[i].Key.Status < items[j].Key.Status
	})
	return items
}

func sortedHTTPDurations(values map[httpDurationKey]*histogram) []struct {
	Key   httpDurationKey
	Value *histogram
} {
	items := make([]struct {
		Key   httpDurationKey
		Value *histogram
	}, 0, len(values))
	for key, value := range values {
		items = append(items, struct {
			Key   httpDurationKey
			Value *histogram
		}{Key: key, Value: value.clone()})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Key.Route != items[j].Key.Route {
			return items[i].Key.Route < items[j].Key.Route
		}
		return items[i].Key.Method < items[j].Key.Method
	})
	return items
}

func sortedProxyRequests(values map[proxyKey]uint64) []struct {
	Key   proxyKey
	Value uint64
} {
	items := make([]struct {
		Key   proxyKey
		Value uint64
	}, 0, len(values))
	for key, value := range values {
		items = append(items, struct {
			Key   proxyKey
			Value uint64
		}{Key: key, Value: value})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Key.BackendID != items[j].Key.BackendID {
			return items[i].Key.BackendID < items[j].Key.BackendID
		}
		if items[i].Key.Protocol != items[j].Key.Protocol {
			return items[i].Key.Protocol < items[j].Key.Protocol
		}
		return items[i].Key.Outcome < items[j].Key.Outcome
	})
	return items
}

func sortedProxyDurations(values map[proxyDurationKey]*histogram) []struct {
	Key   proxyDurationKey
	Value *histogram
} {
	items := make([]struct {
		Key   proxyDurationKey
		Value *histogram
	}, 0, len(values))
	for key, value := range values {
		items = append(items, struct {
			Key   proxyDurationKey
			Value *histogram
		}{Key: key, Value: value.clone()})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Key.BackendID != items[j].Key.BackendID {
			return items[i].Key.BackendID < items[j].Key.BackendID
		}
		return items[i].Key.Protocol < items[j].Key.Protocol
	})
	return items
}

func (h *histogram) clone() *histogram {
	return &histogram{
		Buckets: append([]uint64(nil), h.Buckets...),
		Count:   h.Count,
		Sum:     h.Sum,
	}
}

func sortedWebSocketSessions(values map[websocketKey]uint64) []struct {
	Key   websocketKey
	Value uint64
} {
	items := make([]struct {
		Key   websocketKey
		Value uint64
	}, 0, len(values))
	for key, value := range values {
		items = append(items, struct {
			Key   websocketKey
			Value uint64
		}{Key: key, Value: value})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Key.BackendID != items[j].Key.BackendID {
			return items[i].Key.BackendID < items[j].Key.BackendID
		}
		return items[i].Key.Event < items[j].Key.Event
	})
	return items
}
