// Package config provides unified configuration management for awsocks.
// It supports loading from TOML files, CLI flags, and environment variables
// with a priority system: CLI > profile > defaults > built-in defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// AppConfig represents the complete application configuration loaded from TOML.
type AppConfig struct {
	Defaults Defaults           `toml:"defaults"`
	Profiles map[string]Profile `toml:"profiles"`
}

// Profile represents a named configuration profile for connecting to an EC2 instance.
type Profile struct {
	// Instance identification (one of these is required)
	InstanceID string `toml:"instance-id"`
	Name       string `toml:"name"`

	// AWS settings
	AWSProfile string `toml:"aws-profile"`
	Region     string `toml:"region"`

	// SSH settings
	SSHKey           string `toml:"ssh-key"`
	SSHUser          string `toml:"ssh-user"`
	SSHKeyPassphrase string `toml:"ssh-key-passphrase"`

	// Instance lifecycle
	AutoStart bool `toml:"auto-start"`
	AutoStop  bool `toml:"auto-stop"`

	// Proxy settings
	Mode       string `toml:"mode"`
	Listen     string `toml:"listen"`
	RemotePort int    `toml:"remote-port"`
	Lazy       *bool  `toml:"lazy"`

	// Routing configuration
	Routing *RoutingConfig `toml:"routing"`
}

// Defaults holds default values that apply when not specified in a profile.
type Defaults struct {
	SSHUser    string `toml:"ssh-user"`
	Listen     string `toml:"listen"`
	Mode       string `toml:"mode"`
	RemotePort int    `toml:"remote-port"`
	Lazy       *bool  `toml:"lazy"`

	// Default routing configuration
	Routing *RoutingConfig `toml:"routing"`
}

// RoutingConfig defines how traffic is routed based on destination patterns.
type RoutingConfig struct {
	Default  string   `toml:"default"`
	Proxy    []string `toml:"proxy"`
	Direct   []string `toml:"direct"`
	VMDirect []string `toml:"vm-direct"`
}

// LoadConfig loads configuration from the specified path.
// If the file does not exist, returns an empty config without error.
// If the file exists but is invalid TOML, returns an error.
func LoadConfig(path string) (*AppConfig, error) {
	// Expand ~ to home directory
	path = expandTilde(path)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty config if file doesn't exist
			return &AppConfig{
				Profiles: make(map[string]Profile),
			}, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg AppConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Initialize profiles map if nil
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]Profile)
	}

	return &cfg, nil
}

// GetProfile returns the profile with the given name, if it exists.
func (c *AppConfig) GetProfile(name string) (Profile, bool) {
	profile, exists := c.Profiles[name]
	return profile, exists
}

// DefaultConfigPath returns the default path for the config file.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "awsocks", "config.toml")
}

// expandTilde expands ~ at the start of a path to the user's home directory.
func expandTilde(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}
