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

// ParseWebDriverNewSession decodes a W3C or JsonWire new-session payload and
// returns the ordered list of Request candidates the client is willing to
// accept. W3C firstMatch is expanded: each firstMatch element merged on top of
// alwaysMatch yields a distinct Request. The caller (Selector.SelectFirst)
// picks the first candidate the catalog can honor — this preserves client
// fallback intent (e.g. [chrome, firefox]) instead of collapsing it to the
// first entry.
func ParseWebDriverNewSession(r io.Reader) ([]Request, error) {
	var payload map[string]any
	decoder := json.NewDecoder(io.LimitReader(r, 1<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode webdriver session request: %w", err)
	}

	capsList, err := capabilityCandidatesFromPayload(payload)
	if err != nil {
		return nil, err
	}

	requests := make([]Request, 0, len(capsList))
	for _, caps := range capsList {
		req := requestFromCapabilities(caps)
		if req.BrowserName == "" {
			continue
		}
		requests = append(requests, req)
	}
	if len(requests) == 0 {
		return nil, fmt.Errorf("browserName or deviceName is required")
	}
	return requests, nil
}

func requestFromCapabilities(caps map[string]any) Request {
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
	return req
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

// capabilityCandidatesFromPayload expands W3C capabilities into an ordered
// list: each candidate is alwaysMatch with a single firstMatch entry merged
// over it. When firstMatch is absent the slice has exactly one entry. Legacy
// JsonWire desiredCapabilities also yields a single entry.
func capabilityCandidatesFromPayload(payload map[string]any) ([]map[string]any, error) {
	if capabilities, ok := objectValue(payload, "capabilities"); ok {
		alwaysMatch, _ := objectValue(capabilities, "alwaysMatch")
		firstMatch, hasFirstMatch := arrayValue(capabilities, "firstMatch")

		if !hasFirstMatch || len(firstMatch) == 0 {
			base := copyObject(alwaysMatch)
			if len(base) == 0 {
				return []map[string]any{capabilities}, nil
			}
			return []map[string]any{base}, nil
		}

		candidates := make([]map[string]any, 0, len(firstMatch))
		for _, candidate := range firstMatch {
			caps, ok := candidate.(map[string]any)
			if !ok {
				continue
			}
			merged := copyObject(alwaysMatch)
			for key, value := range caps {
				merged[key] = value
			}
			candidates = append(candidates, merged)
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("firstMatch contains no usable capability entries")
		}
		return candidates, nil
	}
	if desired, ok := objectValue(payload, "desiredCapabilities"); ok {
		return []map[string]any{desired}, nil
	}
	return nil, fmt.Errorf("capabilities or desiredCapabilities is required")
}

func copyObject(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
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

// stringValue extracts a string capability by key. Non-string values (bool,
// number, object) are treated as absent rather than silently stringified —
// "browserName":true must not be interpreted as the literal browser "true".
func stringValue(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	if typed, ok := value.(string); ok {
		return typed
	}
	return ""
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

// SelectFirst tries each candidate Request in order and returns the first one
// for which both the catalog and an available backend pool can be matched.
// When every candidate fails the last encountered error is returned so the
// caller can surface a meaningful diagnosis. Also returns the chosen Request
// so the caller can record the protocol / region actually served.
func (s *Selector) SelectFirst(requests []Request, health BackendHealth) (config.BackendPool, Request, error) {
	if len(requests) == 0 {
		return config.BackendPool{}, Request{}, fmt.Errorf("no candidate requests")
	}
	var lastErr error
	for _, req := range requests {
		pool, err := s.Select(req, health)
		if err == nil {
			return pool, req, nil
		}
		lastErr = err
	}
	return config.BackendPool{}, Request{}, lastErr
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
