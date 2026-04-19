// Package routing owns backend selection and protocol-specific route planning.
package routing

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"

	"gridlane/internal/config"
)

type Request struct {
	Protocol    config.Protocol
	BrowserName string
	Version     string
	Platform    string
	DeviceName  string
	Region      string
}

func ParseWebDriverNewSession(r io.Reader) (Request, error) {
	var payload map[string]any
	decoder := json.NewDecoder(io.LimitReader(r, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return Request{}, fmt.Errorf("decode webdriver session request: %w", err)
	}

	caps, err := capabilitiesFromPayload(payload)
	if err != nil {
		return Request{}, err
	}

	req := Request{
		Protocol:    config.ProtocolWebDriver,
		BrowserName: stringValue(caps, "browserName"),
		Version:     firstStringValue(caps, "browserVersion", "version"),
		Platform:    firstStringValue(caps, "platformName", "platform"),
		DeviceName:  firstStringValue(caps, "deviceName", "appium:deviceName"),
		Region:      firstStringValue(caps, "gridlane:region", "selenoid:region", "region"),
	}
	if req.BrowserName == "" && req.DeviceName != "" {
		req.BrowserName = req.DeviceName
	}
	if req.BrowserName == "" {
		return Request{}, fmt.Errorf("browserName or deviceName is required")
	}
	return req, nil
}

func ParsePlaywrightPath(path string) (Request, error) {
	const prefix = "/playwright/"
	if !strings.HasPrefix(path, prefix) {
		return Request{}, fmt.Errorf("playwright path must start with %s", prefix)
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Request{}, fmt.Errorf("playwright path must be /playwright/<browser>/<version>")
	}
	return Request{
		Protocol:    config.ProtocolPlaywright,
		BrowserName: parts[0],
		Version:     parts[1],
	}, nil
}

func capabilitiesFromPayload(payload map[string]any) (map[string]any, error) {
	if capabilities, ok := objectValue(payload, "capabilities"); ok {
		merged := map[string]any{}
		if alwaysMatch, ok := objectValue(capabilities, "alwaysMatch"); ok {
			for key, value := range alwaysMatch {
				merged[key] = value
			}
		}
		if firstMatch, ok := arrayValue(capabilities, "firstMatch"); ok {
			for _, candidate := range firstMatch {
				if caps, ok := candidate.(map[string]any); ok {
					for key, value := range caps {
						merged[key] = value
					}
					return merged, nil
				}
			}
		}
		if len(merged) > 0 {
			return merged, nil
		}
		return capabilities, nil
	}
	if desired, ok := objectValue(payload, "desiredCapabilities"); ok {
		return desired, nil
	}
	return nil, fmt.Errorf("capabilities or desiredCapabilities is required")
}

func objectValue(values map[string]any, key string) (map[string]any, bool) {
	value, ok := values[key]
	if !ok {
		return nil, false
	}
	object, ok := value.(map[string]any)
	return object, ok
}

func arrayValue(values map[string]any, key string) ([]any, bool) {
	value, ok := values[key]
	if !ok {
		return nil, false
	}
	array, ok := value.([]any)
	return array, ok
}

func stringValue(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(values, key); value != "" {
			return value
		}
	}
	return ""
}

type BackendHealth interface {
	Available(backendID string) bool
}

type Selector struct {
	catalog config.Catalog
	pools   []config.BackendPool
	rand    *rand.Rand
	mu      sync.Mutex
}

func NewSelector(catalog config.Catalog, pools []config.BackendPool, seed uint64) *Selector {
	return &Selector{
		catalog: catalog,
		pools:   append([]config.BackendPool(nil), pools...),
		rand:    rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
	}
}

func (s *Selector) Select(req Request, health BackendHealth) (config.BackendPool, error) {
	if !CatalogSupports(s.catalog, req) {
		return config.BackendPool{}, fmt.Errorf("catalog does not support requested browser")
	}
	candidates := s.candidates(req, health, true)
	if len(candidates) == 0 && req.Region != "" {
		candidates = s.candidates(req, health, false)
	}
	if len(candidates) == 0 {
		return config.BackendPool{}, fmt.Errorf("no backend pool matches request")
	}
	return s.weighted(candidates), nil
}

func (s *Selector) candidates(req Request, health BackendHealth, regionAware bool) []config.BackendPool {
	var matches []config.BackendPool
	for _, pool := range s.pools {
		if regionAware && req.Region != "" && pool.Region != req.Region {
			continue
		}
		if health != nil && !health.Available(pool.ID) {
			continue
		}
		if pool.Weight <= 0 {
			continue
		}
		if !supportsProtocol(pool.Protocols, req.Protocol) {
			continue
		}
		matches = append(matches, pool)
	}
	return matches
}

func (s *Selector) weighted(pools []config.BackendPool) config.BackendPool {
	total := 0
	for _, pool := range pools {
		total += pool.Weight
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pick := s.rand.IntN(total)
	for _, pool := range pools {
		if pick < pool.Weight {
			return pool
		}
		pick -= pool.Weight
	}
	return pools[len(pools)-1]
}

func supportsProtocol(protocols []config.Protocol, protocol config.Protocol) bool {
	for _, candidate := range protocols {
		if candidate == protocol {
			return true
		}
	}
	return false
}

func CatalogSupports(catalog config.Catalog, req Request) bool {
	for _, browser := range catalog.Browsers {
		if !strings.EqualFold(browser.Name, req.BrowserName) {
			continue
		}
		if !supportsProtocol(browser.Protocols, req.Protocol) {
			continue
		}
		if req.Version != "" && !prefixMatchAny(browser.Versions, req.Version) {
			continue
		}
		if req.Platform != "" && len(browser.Platforms) > 0 && !prefixMatchAny(browser.Platforms, req.Platform) {
			continue
		}
		return true
	}
	return false
}

// prefixMatchAny reports whether the client-supplied requested value matches
// any catalog-declared value as a left-anchored, case-insensitive prefix.
// Matching is one-way: the requested string must start with a catalog value
// (the operator declares the coarse identifier, the client narrows it).
// Reverse-direction matches are rejected so that catalog entry "120" cannot
// spuriously satisfy a client asking for "12".
func prefixMatchAny(values []string, requested string) bool {
	requestedLower := strings.ToLower(requested)
	for _, value := range values {
		if strings.HasPrefix(requestedLower, strings.ToLower(value)) {
			return true
		}
	}
	return false
}
