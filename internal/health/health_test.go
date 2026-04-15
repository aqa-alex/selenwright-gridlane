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

func TestManagerReportSuccessResetsFailureState(t *testing.T) {
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
		t.Fatal("Available() = true, want false")
	}
	manager.ReportSuccess("sw-local")
	if !manager.Available("sw-local") {
		t.Fatal("Available() = false, want true after success")
	}
	if got := manager.Snapshot().Backends[0].Failures; got != 0 {
		t.Fatalf("Failures = %d, want 0", got)
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
