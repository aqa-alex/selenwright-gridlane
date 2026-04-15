// Package config owns strict router.json parsing and validation.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

const Version = 1

type Protocol string

const (
	ProtocolWebDriver  Protocol = "webdriver"
	ProtocolPlaywright Protocol = "playwright"
)

// Config is the strict router.json v1 shape.
type Config struct {
	Version      int           `json:"version"`
	Users        []User        `json:"users,omitempty"`
	Guest        *Guest        `json:"guest,omitempty"`
	Catalog      Catalog       `json:"catalog"`
	BackendPools []BackendPool `json:"backend_pools"`
	Admin        Admin         `json:"admin,omitempty"`
}

type User struct {
	Name        string `json:"name"`
	PasswordRef string `json:"password_ref"`
	Quota       Quota  `json:"quota"`
}

type Guest struct {
	Quota Quota `json:"quota"`
}

type Quota struct {
	MaxSessions int `json:"max_sessions"`
}

type Catalog struct {
	Browsers []Browser `json:"browsers"`
}

type Browser struct {
	Name      string     `json:"name"`
	Versions  []string   `json:"versions"`
	Platforms []string   `json:"platforms,omitempty"`
	Protocols []Protocol `json:"protocols"`
}

type BackendPool struct {
	ID          string              `json:"id"`
	Endpoint    string              `json:"endpoint"`
	Region      string              `json:"region"`
	Weight      int                 `json:"weight"`
	Protocols   []Protocol          `json:"protocols"`
	Credentials *BackendCredentials `json:"credentials,omitempty"`
	Health      HealthPolicy        `json:"health,omitempty"`
}

type BackendCredentials struct {
	UsernameRef string `json:"username_ref"`
	PasswordRef string `json:"password_ref"`
}

type HealthPolicy struct {
	Enabled          bool   `json:"enabled"`
	FailureThreshold int    `json:"failure_threshold,omitempty"`
	Cooldown         string `json:"cooldown,omitempty"`
}

type Admin struct {
	TokenRef string `json:"token_ref,omitempty"`
}

func LoadFile(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer func() {
		_ = f.Close()
	}()
	return Load(f)
}

func Load(r io.Reader) (Config, error) {
	var cfg Config
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("router config must contain a single JSON object")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg Config) Validate() error {
	if cfg.Version != Version {
		return fmt.Errorf("version must be %d", Version)
	}
	if len(cfg.Users) == 0 && cfg.Guest == nil {
		return fmt.Errorf("at least one user or guest quota is required")
	}
	if err := validateUsers(cfg.Users); err != nil {
		return err
	}
	if cfg.Guest != nil {
		if err := validateQuota("guest.quota", cfg.Guest.Quota); err != nil {
			return err
		}
	}
	if err := validateCatalog(cfg.Catalog); err != nil {
		return err
	}
	if err := validateBackendPools(cfg.BackendPools); err != nil {
		return err
	}
	if cfg.Admin.TokenRef != "" {
		if err := ValidateSecretRef("admin.token_ref", cfg.Admin.TokenRef); err != nil {
			return err
		}
	}
	return nil
}

func validateUsers(users []User) error {
	seen := map[string]struct{}{}
	for i, user := range users {
		path := fmt.Sprintf("users[%d]", i)
		if user.Name == "" {
			return fmt.Errorf("%s.name is required", path)
		}
		if _, ok := seen[user.Name]; ok {
			return fmt.Errorf("%s.name %q is duplicated", path, user.Name)
		}
		seen[user.Name] = struct{}{}
		if err := ValidateSecretRef(path+".password_ref", user.PasswordRef); err != nil {
			return err
		}
		if err := validateQuota(path+".quota", user.Quota); err != nil {
			return err
		}
	}
	return nil
}

func validateCatalog(catalog Catalog) error {
	if len(catalog.Browsers) == 0 {
		return fmt.Errorf("catalog.browsers must not be empty")
	}
	seen := map[string]struct{}{}
	for i, browser := range catalog.Browsers {
		path := fmt.Sprintf("catalog.browsers[%d]", i)
		if browser.Name == "" {
			return fmt.Errorf("%s.name is required", path)
		}
		if _, ok := seen[browser.Name]; ok {
			return fmt.Errorf("%s.name %q is duplicated", path, browser.Name)
		}
		seen[browser.Name] = struct{}{}
		if len(browser.Versions) == 0 {
			return fmt.Errorf("%s.versions must not be empty", path)
		}
		if err := validateStringList(path+".versions", browser.Versions); err != nil {
			return err
		}
		if err := validateStringList(path+".platforms", browser.Platforms); err != nil {
			return err
		}
		if err := validateProtocols(path+".protocols", browser.Protocols); err != nil {
			return err
		}
	}
	return nil
}

func validateBackendPools(pools []BackendPool) error {
	if len(pools) == 0 {
		return fmt.Errorf("backend_pools must not be empty")
	}
	seen := map[string]struct{}{}
	for i, pool := range pools {
		path := fmt.Sprintf("backend_pools[%d]", i)
		if pool.ID == "" {
			return fmt.Errorf("%s.id is required", path)
		}
		if _, ok := seen[pool.ID]; ok {
			return fmt.Errorf("%s.id %q is duplicated", path, pool.ID)
		}
		seen[pool.ID] = struct{}{}
		if err := validateEndpoint(path+".endpoint", pool.Endpoint); err != nil {
			return err
		}
		if pool.Region == "" {
			return fmt.Errorf("%s.region is required", path)
		}
		if pool.Weight <= 0 {
			return fmt.Errorf("%s.weight must be greater than zero", path)
		}
		if err := validateProtocols(path+".protocols", pool.Protocols); err != nil {
			return err
		}
		if pool.Credentials != nil {
			if err := ValidateSecretRef(path+".credentials.username_ref", pool.Credentials.UsernameRef); err != nil {
				return err
			}
			if err := ValidateSecretRef(path+".credentials.password_ref", pool.Credentials.PasswordRef); err != nil {
				return err
			}
		}
		if err := validateHealth(path+".health", pool.Health); err != nil {
			return err
		}
	}
	return nil
}

func validateQuota(path string, quota Quota) error {
	if quota.MaxSessions <= 0 {
		return fmt.Errorf("%s.max_sessions must be greater than zero", path)
	}
	return nil
}

func validateStringList(path string, values []string) error {
	for i, value := range values {
		if value == "" {
			return fmt.Errorf("%s[%d] must not be empty", path, i)
		}
	}
	return nil
}

func validateProtocols(path string, protocols []Protocol) error {
	if len(protocols) == 0 {
		return fmt.Errorf("%s must not be empty", path)
	}
	for i, protocol := range protocols {
		if protocol != ProtocolWebDriver && protocol != ProtocolPlaywright {
			return fmt.Errorf("%s[%d] has unsupported protocol %q", path, i, protocol)
		}
	}
	return nil
}

func validateEndpoint(path string, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", path)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is invalid: %w", path, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", path)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s host is required", path)
	}
	if parsed.User != nil {
		return fmt.Errorf("%s must not include credentials", path)
	}
	return nil
}

func validateHealth(path string, health HealthPolicy) error {
	if health.FailureThreshold < 0 {
		return fmt.Errorf("%s.failure_threshold must be zero or greater", path)
	}
	if health.Cooldown != "" {
		if _, err := time.ParseDuration(health.Cooldown); err != nil {
			return fmt.Errorf("%s.cooldown is invalid: %w", path, err)
		}
	}
	return nil
}

func ValidateSecretRef(path string, ref string) error {
	if ref == "" {
		return fmt.Errorf("%s is required", path)
	}
	if strings.HasPrefix(ref, "env:") && strings.TrimPrefix(ref, "env:") != "" {
		return nil
	}
	if strings.HasPrefix(ref, "file:") && strings.TrimPrefix(ref, "file:") != "" {
		return nil
	}
	return fmt.Errorf("%s must use env: or file: secret reference", path)
}
