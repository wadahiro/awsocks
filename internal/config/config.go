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
	AutoStart   bool   `toml:"auto-start"`
	AutoStop    bool   `toml:"auto-stop"`
	IdleTimeout string `toml:"idle-timeout"` // e.g., "30m", "1h"

	// Proxy settings
	ProxyNetwork string `toml:"proxy-network"` // "direct" (default) or "vm"
	Listen       string `toml:"listen"`
	HTTPListen   string `toml:"http-listen"` // HTTP CONNECT proxy listen address
	RemotePort   int    `toml:"remote-port"`
	Lazy         *bool  `toml:"lazy"`

	// SSH keepalive
	SSHKeepalive string `toml:"ssh-keepalive"` // e.g., "30s", "0" to disable

	// Upstream proxy configuration
	UpstreamProxy *UpstreamProxyConfig `toml:"upstream-proxy"`

	// Routing configuration
	Routing *RoutingConfig `toml:"routing"`
}

// Defaults holds default values that apply when not specified in a profile.
type Defaults struct {
	SSHUser      string `toml:"ssh-user"`
	Listen       string `toml:"listen"`
	HTTPListen   string `toml:"http-listen"`   // HTTP CONNECT proxy listen address
	ProxyNetwork string `toml:"proxy-network"` // "direct" (default) or "vm"
	RemotePort   int    `toml:"remote-port"`
	Lazy         *bool  `toml:"lazy"`
	IdleTimeout  string `toml:"idle-timeout"`  // e.g., "30m", "1h"
	SSHKeepalive string `toml:"ssh-keepalive"` // e.g., "30s", "0" to disable

	// Upstream proxy configuration
	UpstreamProxy *UpstreamProxyConfig `toml:"upstream-proxy"`

	// Default routing configuration
	Routing *RoutingConfig `toml:"routing"`
}

// UpstreamProxyConfig defines an upstream proxy for SSH tunnel connections.
// Only connections matching the specified patterns are routed through the proxy.
type UpstreamProxyConfig struct {
	URL      string   `toml:"url"`      // e.g., "http://localhost:8080", "socks5://proxy:1080"
	Patterns []string `toml:"patterns"` // e.g., ["*.internal.example.com", "*.partner.example.com"]
}

// DNSRule configures hostname resolution against specific DNS servers,
// overriding whatever resolver the connect route would otherwise use.
//
// Rules are evaluated in order and the first one matching a hostname wins.
type DNSRule struct {
	// Via is the route carrying the DNS query itself: "proxy" (default),
	// "direct", or "vm-direct". It is independent of the route used to reach
	// the resolved address, because which combination is correct depends on
	// the network topology that only the operator knows.
	Via string `toml:"via"`

	// Servers lists DNS servers as "IP" or "IP:port" (port defaults to 53),
	// tried in order. Hostnames are rejected: resolving them would require
	// the resolver being configured.
	Servers []string `toml:"servers"`

	// Patterns limits the rule to matching hostnames. Empty matches all.
	Patterns []string `toml:"patterns"`

	// Timeout bounds a single query against a single server (e.g. "3s").
	Timeout string `toml:"timeout"`

	// MinTTL and MaxTTL clamp the TTL taken from responses.
	MinTTL string `toml:"min-ttl"`
	MaxTTL string `toml:"max-ttl"`

	// NegativeTTL is how long NXDOMAIN and empty answers are cached.
	NegativeTTL string `toml:"negative-ttl"`

	// OnFailure selects behavior when resolution fails: "fallthrough"
	// (default) hands the hostname to the connect route unchanged, "fail"
	// returns the error to the client.
	OnFailure string `toml:"on-failure"`

	// Prefer selects the address family: "ipv4" (default) or "ipv6".
	Prefer string `toml:"prefer"`
}

// RoutingConfig defines how traffic is routed based on destination patterns.
type RoutingConfig struct {
	Default  string            `toml:"default"`
	Proxy    []string          `toml:"proxy"`
	Direct   []string          `toml:"direct"`
	VMDirect []string          `toml:"vm-direct"`
	Hosts    map[string]string `toml:"hosts"`

	// DNS lists hostname resolution rules, evaluated in order.
	// Static Hosts entries take precedence over these.
	DNS []DNSRule `toml:"dns"`

	// PreConnectDirect lists hosts to route directly (bypassing proxy)
	// only while the proxy backend has not finished connecting yet.
	// Once the backend is active, these hosts follow the normal routing decision.
	PreConnectDirect []string `toml:"pre-connect-direct"`
	// PreConnectVMDirect is the vm-direct equivalent of PreConnectDirect.
	PreConnectVMDirect []string `toml:"pre-connect-vm-direct"`
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
