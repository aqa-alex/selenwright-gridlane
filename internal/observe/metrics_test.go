package observe

import (
	"strings"
	"testing"
	"time"
)

func TestRouteLabelRedactsSessionIDs(t *testing.T) {
	tests := map[string]string{
		"/wd/hub/session/r1_token_secret/url":        "/wd/hub/session/:session",
		"/playwright/chrome/stable":                  "/playwright/:browser/:version",
		"/logs/r1_token_secret":                      "/logs/:session",
		"/download/r1_token_secret/report.txt":       "/download/:session",
		"/history/settings":                          "/history/settings",
		"/unexpected/r1_token_should_not_be_a_label": "other",
	}
	for path, want := range tests {
		if got := RouteLabel(path); got != want {
			t.Fatalf("RouteLabel(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestMetricsPrometheusOutput(t *testing.T) {
	metrics := NewMetrics()
	metrics.RecordHTTPRequest("GET", "/logs/r1_secret", 200, 12*time.Millisecond)
	metrics.RecordProxyRequest("playwright", "sw-a", "success", 20*time.Millisecond)
	metrics.RecordWebSocketSession("sw-a", "upgraded")

	var out strings.Builder
	metrics.WritePrometheus(&out, nil)
	body := out.String()

	for _, want := range []string{
		`gridlane_http_requests_total{method="GET",route="/logs/:session",status="200"} 1`,
		`gridlane_proxy_requests_total{protocol="playwright",backend="sw-a",outcome="success"} 1`,
		`gridlane_websocket_sessions_total{backend="sw-a",event="upgraded"} 1`,
		`gridlane_http_request_duration_seconds_bucket{method="GET",route="/logs/:session",le="+Inf"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "r1_secret") {
		t.Fatalf("metrics output leaked session ID:\n%s", body)
	}
}
