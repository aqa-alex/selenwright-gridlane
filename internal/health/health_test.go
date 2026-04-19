package health

import (
	"testing"
	"time"

	"gridlane/internal/config"
)

func TestManagerHealthCooldown(t *testing.T) {
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

func TestManagerDisabledHealthIgnoresReportSuccess(t *testing.T) {
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
