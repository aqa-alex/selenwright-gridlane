package health

import (
	"sync"
	"testing"
	"time"

	"gridlane/internal/config"
)

func TestManagerHealthCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	manager := NewManagerWithClock([]config.BackendPool{{
		ID:        "sw-local",
		Endpoint:  "http://127.0.0.1:4444",
		Region:    "eu",
		Protocols: []config.Protocol{config.ProtocolWebDriver},
		Health:    config.HealthPolicy{Enabled: true, FailureThreshold: 2, Cooldown: "10s"},
	}}, func() time.Time { return now })

	if !manager.Available("sw-local") {
		t.Fatal("Available() = false, want true before failures")
	}
	manager.ReportFailure("sw-local")
	if !manager.Available("sw-local") {
		t.Fatal("Available() = false, want true below threshold")
	}
	manager.ReportFailure("sw-local")
	if manager.Available("sw-local") {
		t.Fatal("Available() = true, want false after threshold")
	}

	snapshot := manager.Snapshot()
	if snapshot.Status != "degraded" {
		t.Fatalf("Status = %q, want degraded", snapshot.Status)
	}
	if snapshot.Backends[0].UnhealthyUntil == "" {
		t.Fatal("UnhealthyUntil is empty, want cooldown timestamp")
	}

	now = now.Add(11 * time.Second)
	if !manager.Available("sw-local") {
		t.Fatal("Available() = false, want true after cooldown")
	}
}

func TestManagerReportSuccessResetsFailuresBelowThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	manager := NewManagerWithClock([]config.BackendPool{{
		ID:        "sw-local",
		Endpoint:  "http://127.0.0.1:4444",
		Region:    "eu",
		Protocols: []config.Protocol{config.ProtocolWebDriver},
		Health:    config.HealthPolicy{Enabled: true, FailureThreshold: 3, Cooldown: "10s"},
	}}, func() time.Time { return now })

	manager.ReportFailure("sw-local")
	manager.ReportFailure("sw-local")
	if !manager.Available("sw-local") {
		t.Fatal("Available() = false, want true below threshold")
	}
	if got := manager.Snapshot().Backends[0].Failures; got != 2 {
		t.Fatalf("Failures = %d, want 2 before success", got)
	}

	manager.ReportSuccess("sw-local")

	if !manager.Available("sw-local") {
		t.Fatal("Available() = false, want true after success below threshold")
	}
	if got := manager.Snapshot().Backends[0].Failures; got != 0 {
		t.Fatalf("Failures = %d, want 0 after success below threshold", got)
	}
}

func TestManagerReportSuccessDoesNotBypassCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	manager := NewManagerWithClock([]config.BackendPool{{
		ID:        "sw-local",
		Endpoint:  "http://127.0.0.1:4444",
		Region:    "eu",
		Protocols: []config.Protocol{config.ProtocolWebDriver},
		Health:    config.HealthPolicy{Enabled: true, FailureThreshold: 2, Cooldown: "10s"},
	}}, func() time.Time { return now })

	manager.ReportFailure("sw-local")
	manager.ReportFailure("sw-local")
	if manager.Available("sw-local") {
		t.Fatal("Available() = true, want false after threshold")
	}

	manager.ReportSuccess("sw-local")

	if manager.Available("sw-local") {
		t.Fatal("Available() = true, want false: a single success must not heal during cooldown")
	}
	if got := manager.Snapshot().Backends[0].Failures; got != 2 {
		t.Fatalf("Failures = %d, want 2 to remain during cooldown", got)
	}
}

func TestManagerReportSuccessAfterCooldownResetsState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	manager := NewManagerWithClock([]config.BackendPool{{
		ID:        "sw-local",
		Endpoint:  "http://127.0.0.1:4444",
		Region:    "eu",
		Protocols: []config.Protocol{config.ProtocolWebDriver},
		Health:    config.HealthPolicy{Enabled: true, FailureThreshold: 1, Cooldown: "10s"},
	}}, func() time.Time { return now })

	manager.ReportFailure("sw-local")
	if manager.Available("sw-local") {
		t.Fatal("Available() = true, want false after threshold")
	}

	now = now.Add(11 * time.Second)
	manager.ReportSuccess("sw-local")

	if !manager.Available("sw-local") {
		t.Fatal("Available() = false, want true after cooldown + success")
	}
	snapshot := manager.Snapshot().Backends[0]
	if snapshot.Failures != 0 {
		t.Fatalf("Failures = %d, want 0 after post-cooldown success", snapshot.Failures)
	}
	if snapshot.UnhealthyUntil != "" {
		t.Fatalf("UnhealthyUntil = %q, want empty after post-cooldown success", snapshot.UnhealthyUntil)
	}
}

func TestManagerDisabledHealthDoesNotTrip(t *testing.T) {
	t.Parallel()
	manager := NewManagerWithClock([]config.BackendPool{{
		ID:        "sw-local",
		Endpoint:  "http://127.0.0.1:4444",
		Region:    "eu",
		Protocols: []config.Protocol{config.ProtocolWebDriver},
		Health:    config.HealthPolicy{Enabled: false, FailureThreshold: 1, Cooldown: "10s"},
	}}, func() time.Time { return time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC) })

	manager.ReportFailure("sw-local")
	if !manager.Available("sw-local") {
		t.Fatal("Available() = false, want true when health policy is disabled")
	}
}

func TestManagerSnapshotClearsExpiredCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	manager := NewManagerWithClock([]config.BackendPool{{
		ID:        "sw-local",
		Endpoint:  "http://127.0.0.1:4444",
		Region:    "eu",
		Protocols: []config.Protocol{config.ProtocolWebDriver},
		Health:    config.HealthPolicy{Enabled: true, FailureThreshold: 1, Cooldown: "10s"},
	}}, func() time.Time { return now })

	manager.ReportFailure("sw-local")
	if snap := manager.Snapshot().Backends[0]; snap.UnhealthyUntil == "" || snap.Failures != 1 {
		t.Fatalf("mid-cooldown snapshot = %+v, want failures=1 and UnhealthyUntil set", snap)
	}

	now = now.Add(11 * time.Second)

	snap := manager.Snapshot().Backends[0]
	if snap.UnhealthyUntil != "" {
		t.Fatalf("post-cooldown UnhealthyUntil = %q, want empty (stale state must be cleared)", snap.UnhealthyUntil)
	}
	if snap.Failures != 0 {
		t.Fatalf("post-cooldown Failures = %d, want 0 after expiry cleanup", snap.Failures)
	}
	if !snap.Available {
		t.Fatal("post-cooldown Available = false, want true")
	}
}

func TestManagerConcurrentReports(t *testing.T) {
	t.Parallel()
	// Stresses Manager under -race: N goroutines hammer ReportFailure,
	// ReportSuccess and Available at once. Passes means the mutex keeps the
	// backend map and state counters coherent.
	pools := make([]config.BackendPool, 0, 4)
	for _, id := range []string{"a", "b", "c", "d"} {
		pools = append(pools, config.BackendPool{
			ID:        id,
			Endpoint:  "http://127.0.0.1:4444",
			Region:    "eu",
			Protocols: []config.Protocol{config.ProtocolWebDriver},
			Health:    config.HealthPolicy{Enabled: true, FailureThreshold: 3, Cooldown: "100ms"},
		})
	}
	manager := NewManager(pools)

	const workers = 16
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(w int) {
			defer wg.Done()
			id := pools[w%len(pools)].ID
			for i := 0; i < iterations; i++ {
				switch i % 3 {
				case 0:
					manager.ReportFailure(id)
				case 1:
					manager.ReportSuccess(id)
				case 2:
					_ = manager.Available(id)
				}
			}
		}(worker)
	}
	wg.Wait()

	// Sanity: every pool must still be addressable from the public API.
	snap := manager.Snapshot()
	if len(snap.Backends) != len(pools) {
		t.Fatalf("Snapshot.Backends = %d, want %d", len(snap.Backends), len(pools))
	}
}

func TestManagerDisabledHealthIgnoresReportSuccess(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)
	manager := NewManagerWithClock([]config.BackendPool{{
		ID:        "sw-local",
		Endpoint:  "http://127.0.0.1:4444",
		Region:    "eu",
		Protocols: []config.Protocol{config.ProtocolWebDriver},
		Health:    config.HealthPolicy{Enabled: false, FailureThreshold: 1, Cooldown: "10s"},
	}}, func() time.Time { return now })

	manager.ReportSuccess("sw-local")
	if got := manager.Snapshot().Backends[0].Failures; got != 0 {
		t.Fatalf("Failures = %d, want 0 for disabled pool regardless", got)
	}
}
