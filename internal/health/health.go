// Package health owns backend health state and cooldown policy.
package health

import (
	"sync"
	"time"

	"gridlane/internal/config"
)

const defaultCooldown = 30 * time.Second

type Manager struct {
	mu       sync.Mutex
	now      func() time.Time
	backends map[string]*backendState
}

type backendState struct {
	id               string
	region           string
	endpoint         string
	protocols        []config.Protocol
	enabled          bool
	failureThreshold int
	cooldown         time.Duration
	failures         int
	unhealthyUntil   time.Time
}

func NewManager(pools []config.BackendPool) *Manager {
	return NewManagerWithClock(pools, time.Now)
}

func NewManagerWithClock(pools []config.BackendPool, now func() time.Time) *Manager {
	manager := &Manager{
		now:      now,
		backends: make(map[string]*backendState, len(pools)),
	}
	for _, pool := range pools {
		threshold := pool.Health.FailureThreshold
		if threshold <= 0 {
			threshold = 1
		}
		cooldown := defaultCooldown
		if pool.Health.Cooldown != "" {
			if parsed, err := time.ParseDuration(pool.Health.Cooldown); err == nil {
				cooldown = parsed
			}
		}
		manager.backends[pool.ID] = &backendState{
			id:               pool.ID,
			region:           pool.Region,
			endpoint:         pool.Endpoint,
			protocols:        append([]config.Protocol(nil), pool.Protocols...),
			enabled:          pool.Health.Enabled,
			failureThreshold: threshold,
			cooldown:         cooldown,
		}
	}
	return manager
}

func (m *Manager) Available(backendID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.backends[backendID]
	if !ok {
		return false
	}
	return state.available(m.now())
}

func (m *Manager) ReportSuccess(backendID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.backends[backendID]
	if !ok {
		return
	}
	if !state.enabled {
		return
	}
	if !state.unhealthyUntil.IsZero() && state.unhealthyUntil.After(m.now()) {
		return
	}
	state.failures = 0
	state.unhealthyUntil = time.Time{}
}

func (m *Manager) ReportFailure(backendID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.backends[backendID]
	if !ok {
		return
	}
	if !state.enabled {
		return
	}
	state.failures++
	if state.failures >= state.failureThreshold {
		state.unhealthyUntil = m.now().Add(state.cooldown)
	}
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	backends := make([]BackendSnapshot, 0, len(m.backends))
	for _, state := range m.backends {
		snapshot := BackendSnapshot{
			ID:               state.id,
			Region:           state.region,
			Endpoint:         state.endpoint,
			Protocols:        append([]config.Protocol(nil), state.protocols...),
			HealthEnabled:    state.enabled,
			Available:        state.available(now),
			Failures:         state.failures,
			FailureThreshold: state.failureThreshold,
		}
		if !state.unhealthyUntil.IsZero() && state.unhealthyUntil.After(now) {
			snapshot.UnhealthyUntil = state.unhealthyUntil.UTC().Format(time.RFC3339)
		}
		backends = append(backends, snapshot)
	}
	return Snapshot{
		Service:  "gridlane",
		Status:   overallStatus(backends),
		Backends: backends,
	}
}

type Snapshot struct {
	Service  string            `json:"service"`
	Status   string            `json:"status"`
	Backends []BackendSnapshot `json:"backends"`
}

// PublicSnapshot is the response shape returned to unauthenticated callers of
// /status. It intentionally omits per-backend endpoints, regions, protocol
// lists, and failure thresholds so that the public surface does not leak the
// backend topology or the router's health policy.
type PublicSnapshot struct {
	Service        string `json:"service"`
	Status         string `json:"status"`
	BackendCount   int    `json:"backend_count"`
	AvailableCount int    `json:"available_count"`
}

func (m *Manager) PublicSnapshot() PublicSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	available := 0
	backends := make([]BackendSnapshot, 0, len(m.backends))
	for _, state := range m.backends {
		snapshot := BackendSnapshot{Available: state.available(now)}
		if snapshot.Available {
			available++
		}
		backends = append(backends, snapshot)
	}
	return PublicSnapshot{
		Service:        "gridlane",
		Status:         overallStatus(backends),
		BackendCount:   len(backends),
		AvailableCount: available,
	}
}

type BackendSnapshot struct {
	ID               string            `json:"id"`
	Region           string            `json:"region"`
	Endpoint         string            `json:"endpoint"`
	Protocols        []config.Protocol `json:"protocols"`
	HealthEnabled    bool              `json:"health_enabled"`
	Available        bool              `json:"available"`
	Failures         int               `json:"failures"`
	FailureThreshold int               `json:"failure_threshold"`
	UnhealthyUntil   string            `json:"unhealthy_until,omitempty"`
}

func (s *backendState) available(now time.Time) bool {
	return s.unhealthyUntil.IsZero() || !s.unhealthyUntil.After(now)
}

func overallStatus(backends []BackendSnapshot) string {
	if len(backends) == 0 {
		return "degraded"
	}
	for _, backend := range backends {
		if backend.Available {
			return "ok"
		}
	}
	return "degraded"
}
