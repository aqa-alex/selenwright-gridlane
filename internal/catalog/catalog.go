// Package catalog owns the validated user, quota, browser, and backend catalog.
package catalog

import (
	"fmt"

	"gridlane/internal/config"
)

type Catalog struct {
	cfg         config.Config
	userNames   map[string]struct{}
	backendByID map[string]config.BackendPool
}

func New(cfg config.Config) (*Catalog, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	userNames := make(map[string]struct{}, len(cfg.Users))
	for _, user := range cfg.Users {
		userNames[user.Name] = struct{}{}
	}
	backendByID := make(map[string]config.BackendPool, len(cfg.BackendPools))
	for _, pool := range cfg.BackendPools {
		backendByID[pool.ID] = pool
	}
	return &Catalog{cfg: cfg, userNames: userNames, backendByID: backendByID}, nil
}

func (c *Catalog) Config() config.Config {
	return c.cfg
}

func (c *Catalog) SanitizedConfig(path string) SanitizedConfig {
	users := make([]SanitizedUser, 0, len(c.cfg.Users))
	for _, user := range c.cfg.Users {
		users = append(users, SanitizedUser{
			Name:               user.Name,
			Quota:              user.Quota,
			PasswordConfigured: user.PasswordRef != "",
		})
	}

	var guest *SanitizedGuest
	if c.cfg.Guest != nil {
		guest = &SanitizedGuest{Quota: c.cfg.Guest.Quota}
	}

	pools := make([]SanitizedBackendPool, 0, len(c.cfg.BackendPools))
	for _, pool := range c.cfg.BackendPools {
		pools = append(pools, SanitizedBackendPool{
			ID:                    pool.ID,
			Endpoint:              pool.Endpoint,
			Region:                pool.Region,
			Weight:                pool.Weight,
			Protocols:             pool.Protocols,
			CredentialsConfigured: pool.Credentials != nil,
			Health:                pool.Health,
		})
	}

	return SanitizedConfig{
		Service:      "gridlane",
		Version:      c.cfg.Version,
		ConfigPath:   path,
		Users:        users,
		Guest:        guest,
		Catalog:      c.cfg.Catalog,
		BackendPools: pools,
		Admin: SanitizedAdmin{
			TokenConfigured: c.cfg.Admin.TokenRef != "",
		},
	}
}

func (c *Catalog) Quota() QuotaView {
	users := make([]QuotaUser, 0, len(c.cfg.Users))
	for _, user := range c.cfg.Users {
		users = append(users, QuotaUser{
			Name:  user.Name,
			Quota: user.Quota,
		})
	}

	var guest *QuotaGuest
	if c.cfg.Guest != nil {
		guest = &QuotaGuest{Quota: c.cfg.Guest.Quota}
	}

	return QuotaView{
		Service: "gridlane",
		Users:   users,
		Guest:   guest,
	}
}

func (c *Catalog) UserExists(name string) bool {
	_, ok := c.userNames[name]
	return ok
}

func (c *Catalog) BackendByID(id string) (config.BackendPool, error) {
	pool, ok := c.backendByID[id]
	if !ok {
		return config.BackendPool{}, fmt.Errorf("backend pool %q not found", id)
	}
	return pool, nil
}

type SanitizedConfig struct {
	Service      string                 `json:"service"`
	Version      int                    `json:"version"`
	ConfigPath   string                 `json:"config_path"`
	Users        []SanitizedUser        `json:"users,omitempty"`
	Guest        *SanitizedGuest        `json:"guest,omitempty"`
	Catalog      config.Catalog         `json:"catalog"`
	BackendPools []SanitizedBackendPool `json:"backend_pools"`
	Admin        SanitizedAdmin         `json:"admin"`
}

type SanitizedUser struct {
	Name               string       `json:"name"`
	Quota              config.Quota `json:"quota"`
	PasswordConfigured bool         `json:"password_configured"`
}

type SanitizedGuest struct {
	Quota config.Quota `json:"quota"`
}

type SanitizedBackendPool struct {
	ID                    string              `json:"id"`
	Endpoint              string              `json:"endpoint"`
	Region                string              `json:"region"`
	Weight                int                 `json:"weight"`
	Protocols             []config.Protocol   `json:"protocols"`
	CredentialsConfigured bool                `json:"credentials_configured"`
	Health                config.HealthPolicy `json:"health,omitempty"`
}

type SanitizedAdmin struct {
	TokenConfigured bool `json:"token_configured"`
}

type QuotaView struct {
	Service string      `json:"service"`
	Users   []QuotaUser `json:"users,omitempty"`
	Guest   *QuotaGuest `json:"guest,omitempty"`
}

type QuotaUser struct {
	Name  string       `json:"name"`
	Quota config.Quota `json:"quota"`
}

type QuotaGuest struct {
	Quota config.Quota `json:"quota"`
}
