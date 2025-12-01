package config

import (
	"fmt"
	"os"
	"strings"
)

// Strategy represents the deletion strategy
type Strategy string

const (
	StrategyFull       Strategy = "full"
	StrategyUpsertOnly Strategy = "upsert-only"
)

// Config holds the application configuration
type Config struct {
	CloudflareAPIToken string
	Strategy           Strategy
	KubeconfigPath     string
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

	return cfg, nil
}

// ShouldDelete returns true if records should be deleted (full strategy)
func (c *Config) ShouldDelete() bool {
	return c.Strategy == StrategyFull
}
