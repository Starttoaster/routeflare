package config

import (
	"fmt"
	"os"
	"strings"
)

// Strategy represents the deletion strategy
type Strategy string

const (
	// StrategyFull strategy informs routeflare it should fully manage DNS records for HTTPRoutes
	StrategyFull Strategy = "full"
	// StrategyUpsertOnly strategy informs routeflare it should only create and update DNS records for HTTPRoutes
	StrategyUpsertOnly Strategy = "upsert-only"
)

// Config holds the application configuration
type Config struct {
	CloudflareAPIToken string
	Strategy           Strategy
	KubeconfigPath     string
	RecordOwnerID      string
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	cfg := &Config{}

	// CLOUDFLARE_API_TOKEN is required
	cfg.CloudflareAPIToken = os.Getenv("CLOUDFLARE_API_TOKEN")
	if cfg.CloudflareAPIToken == "" {
		return nil, fmt.Errorf("CLOUDFLARE_API_TOKEN environment variable is required")
	}

	// STRATEGY is optional, defaults to "full"
	strategyStr := strings.ToLower(os.Getenv("STRATEGY"))
	if strategyStr == "" {
		cfg.Strategy = StrategyFull
	} else {
		cfg.Strategy = Strategy(strategyStr)
		if cfg.Strategy != StrategyFull && cfg.Strategy != StrategyUpsertOnly {
			return nil, fmt.Errorf("STRATEGY must be either 'full' or 'upsert-only', got: %s", strategyStr)
		}
	}

	// KUBECONFIG is optional
	cfg.KubeconfigPath = os.Getenv("KUBECONFIG")

	// RECORD_OWNER_ID is optional, defaults to "routeflare"
	cfg.RecordOwnerID = os.Getenv("RECORD_OWNER_ID")
	if cfg.RecordOwnerID == "" {
		cfg.RecordOwnerID = "routeflare"
	}

	return cfg, nil
}

// ShouldDelete returns true if records should be deleted (full strategy)
func (c *Config) ShouldDelete() bool {
	return c.Strategy == StrategyFull
}
