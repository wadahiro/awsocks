package routing

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config represents the routing configuration loaded from TOML
type Config struct {
	Default  string   `toml:"default"`
	Proxy    []string `toml:"proxy"`
	Direct   []string `toml:"direct"`
	VMDirect []string `toml:"vm-direct"`
}

// LoadConfig loads routing configuration from a TOML file
func LoadConfig(path string) (*Config, error) {
	// Expand ~ to home directory
	if len(path) > 0 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		path = filepath.Join(home, path[1:])
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Validate default route
	if cfg.Default == "" {
		cfg.Default = "proxy"
	}

	defaultRoute := Route(cfg.Default)
	if !defaultRoute.IsValid() {
		return nil, fmt.Errorf("invalid default route: %s", cfg.Default)
	}

	return &cfg, nil
}

// DefaultConfigPath returns the default path for the routing config file
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "awsocks", "config.toml")
}
