package routing

import (
	"strings"
	"testing"

	"gridlane/internal/config"
)

func TestParseWebDriverW3CAlwaysMatch(t *testing.T) {
	req, err := ParseWebDriverNewSession(strings.NewReader(`{
		"capabilities": {
			"alwaysMatch": {
				"browserName": "chrome",
				"browserVersion": "128",
				"platformName": "linux",
				"gridlane:region": "eu"
			}
		}
	}`))
	if err != nil {
		t.Fatalf("ParseWebDriverNewSession() error = %v", err)
	}
	if req.Protocol != config.ProtocolWebDriver {
		t.Fatalf("Protocol = %q, want webdriver", req.Protocol)
	}
	if req.BrowserName != "chrome" {
		t.Fatalf("BrowserName = %q, want chrome", req.BrowserName)
	}
	if req.Version != "128" {
		t.Fatalf("Version = %q, want 128", req.Version)
	}
	if req.Platform != "linux" {
		t.Fatalf("Platform = %q, want linux", req.Platform)
	}
	if req.Region != "eu" {
		t.Fatalf("Region = %q, want eu", req.Region)
	}
}

func TestParseWebDriverJSONWireDeviceNameFallback(t *testing.T) {
	req, err := ParseWebDriverNewSession(strings.NewReader(`{
		"desiredCapabilities": {
			"appium:deviceName": "iPhone 15",
			"version": "17",
			"platform": "ios"
		}
	}`))
	if err != nil {
		t.Fatalf("ParseWebDriverNewSession() error = %v", err)
	}
	if req.BrowserName != "iPhone 15" {
		t.Fatalf("BrowserName = %q, want device fallback", req.BrowserName)
	}
	if req.DeviceName != "iPhone 15" {
		t.Fatalf("DeviceName = %q, want iPhone 15", req.DeviceName)
	}
	if req.Version != "17" {
		t.Fatalf("Version = %q, want 17", req.Version)
	}
	if req.Platform != "ios" {
		t.Fatalf("Platform = %q, want ios", req.Platform)
	}
}

func TestParseWebDriverW3CMergesAlwaysAndFirstMatch(t *testing.T) {
	req, err := ParseWebDriverNewSession(strings.NewReader(`{
		"capabilities": {
			"alwaysMatch": {"platformName": "linux"},
			"firstMatch": [{"browserName": "chrome", "browserVersion": "128"}]
		}
	}`))
	if err != nil {
		t.Fatalf("ParseWebDriverNewSession() error = %v", err)
	}
	if req.BrowserName != "chrome" {
		t.Fatalf("BrowserName = %q, want chrome", req.BrowserName)
	}
	if req.Version != "128" {
		t.Fatalf("Version = %q, want 128", req.Version)
	}
	if req.Platform != "linux" {
		t.Fatalf("Platform = %q, want linux", req.Platform)
	}
}

func TestParsePlaywrightPath(t *testing.T) {
	req, err := ParsePlaywrightPath("/playwright/chromium/1.56")
	if err != nil {
		t.Fatalf("ParsePlaywrightPath() error = %v", err)
	}
	if req.Protocol != config.ProtocolPlaywright {
		t.Fatalf("Protocol = %q, want playwright", req.Protocol)
	}
	if req.BrowserName != "chromium" {
		t.Fatalf("BrowserName = %q, want chromium", req.BrowserName)
	}
	if req.Version != "1.56" {
		t.Fatalf("Version = %q, want 1.56", req.Version)
	}
}

func TestSelectorRegionFallbackAndHealthFiltering(t *testing.T) {
	pools := []config.BackendPool{
		{ID: "us", Region: "us", Weight: 1, Protocols: []config.Protocol{config.ProtocolWebDriver}},
		{ID: "eu", Region: "eu", Weight: 1, Protocols: []config.Protocol{config.ProtocolWebDriver}},
	}
	selector := NewSelector(sampleCatalog(), pools, 1)

	selected, err := selector.Select(Request{Protocol: config.ProtocolWebDriver, BrowserName: "chrome", Region: "eu"}, fixedHealth{"eu": false, "us": true})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selected.ID != "us" {
		t.Fatalf("selected backend = %q, want fallback to us", selected.ID)
	}
}

func TestSelectorWeightedSelection(t *testing.T) {
	pools := []config.BackendPool{
		{ID: "zero", Region: "eu", Weight: 0, Protocols: []config.Protocol{config.ProtocolWebDriver}},
		{ID: "winner", Region: "eu", Weight: 10, Protocols: []config.Protocol{config.ProtocolWebDriver}},
	}
	selector := NewSelector(sampleCatalog(), pools, 1)

	selected, err := selector.Select(Request{Protocol: config.ProtocolWebDriver, BrowserName: "chrome", Region: "eu"}, nil)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selected.ID != "winner" {
		t.Fatalf("selected backend = %q, want winner", selected.ID)
	}
}

func TestCatalogSupportsVersionAndPlatformPrefix(t *testing.T) {
	req := Request{
		Protocol:    config.ProtocolWebDriver,
		BrowserName: "chrome",
		Version:     "128.0",
		Platform:    "linux-amd64",
	}
	if !CatalogSupports(sampleCatalog(), req) {
		t.Fatal("CatalogSupports() = false, want true")
	}
}

func TestCatalogPrefixMatchIsOneWay(t *testing.T) {
	catalog := config.Catalog{Browsers: []config.Browser{{
		Name:      "chrome",
		Versions:  []string{"120"},
		Platforms: []string{"linux"},
		Protocols: []config.Protocol{config.ProtocolWebDriver},
	}}}

	req := Request{
		Protocol:    config.ProtocolWebDriver,
		BrowserName: "chrome",
		Version:     "12",
		Platform:    "linux",
	}
	if CatalogSupports(catalog, req) {
		t.Fatal("CatalogSupports(version=12, catalog=[120]) = true, want false: client prefix must not match wider catalog value")
	}
}

func TestCatalogPrefixMatchAcceptsNarrowerClient(t *testing.T) {
	catalog := config.Catalog{Browsers: []config.Browser{{
		Name:      "chrome",
		Versions:  []string{"12"},
		Platforms: []string{"linux"},
		Protocols: []config.Protocol{config.ProtocolWebDriver},
	}}}

	req := Request{
		Protocol:    config.ProtocolWebDriver,
		BrowserName: "chrome",
		Version:     "120.0.6099.109",
		Platform:    "linux-amd64",
	}
	if !CatalogSupports(catalog, req) {
		t.Fatal("CatalogSupports(version=120.0..., catalog=[12]) = false, want true: client value widening catalog prefix must match")
	}
}

func TestSelectorRejectsUnsupportedBrowser(t *testing.T) {
	selector := NewSelector(sampleCatalog(), []config.BackendPool{
		{ID: "local", Region: "eu", Weight: 1, Protocols: []config.Protocol{config.ProtocolWebDriver}},
	}, 1)

	_, err := selector.Select(Request{Protocol: config.ProtocolWebDriver, BrowserName: "safari"}, nil)
	if err == nil {
		t.Fatal("Select() error = nil, want unsupported browser")
	}
}

func sampleCatalog() config.Catalog {
	return config.Catalog{Browsers: []config.Browser{{
		Name:      "chrome",
		Versions:  []string{"128"},
		Platforms: []string{"linux"},
		Protocols: []config.Protocol{config.ProtocolWebDriver, config.ProtocolPlaywright},
	}}}
}

type fixedHealth map[string]bool

func (h fixedHealth) Available(backendID string) bool {
	return h[backendID]
}
