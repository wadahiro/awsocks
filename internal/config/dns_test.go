package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	return path
}

func TestLoadConfig_DNSRules(t *testing.T) {
	path := writeConfig(t, `
[[defaults.routing.dns]]
via = "proxy"
servers = ["10.0.0.2:53"]
patterns = ["*.internal.example.com"]
timeout = "5s"
min-ttl = "15s"
max-ttl = "2m"
negative-ttl = "3s"
on-failure = "fail"
prefer = "ipv6"

[[defaults.routing.dns]]
via = "direct"
servers = ["192.168.1.1"]

[profiles.prod]
instance-id = "i-123"

[[profiles.prod.routing.dns]]
via = "vm-direct"
servers = ["10.1.0.2"]
patterns = ["*.prod.example.com"]
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	require.NotNil(t, cfg.Defaults.Routing)
	require.Len(t, cfg.Defaults.Routing.DNS, 2)

	first := cfg.Defaults.Routing.DNS[0]
	assert.Equal(t, "proxy", first.Via)
	assert.Equal(t, []string{"10.0.0.2:53"}, first.Servers)
	assert.Equal(t, []string{"*.internal.example.com"}, first.Patterns)
	assert.Equal(t, "5s", first.Timeout)
	assert.Equal(t, "15s", first.MinTTL)
	assert.Equal(t, "2m", first.MaxTTL)
	assert.Equal(t, "3s", first.NegativeTTL)
	assert.Equal(t, "fail", first.OnFailure)
	assert.Equal(t, "ipv6", first.Prefer)

	assert.Equal(t, "direct", cfg.Defaults.Routing.DNS[1].Via)

	prod, ok := cfg.GetProfile("prod")
	require.True(t, ok)
	require.NotNil(t, prod.Routing)
	require.Len(t, prod.Routing.DNS, 1)
	assert.Equal(t, "vm-direct", prod.Routing.DNS[0].Via)
}

func TestLoadConfig_NoDNSRules(t *testing.T) {
	path := writeConfig(t, `
[defaults.routing]
default = "proxy"
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	require.NotNil(t, cfg.Defaults.Routing)
	assert.Empty(t, cfg.Defaults.Routing.DNS)
}

func TestMerge_DNSInheritedFromDefaults(t *testing.T) {
	defaults := &Defaults{
		Routing: &RoutingConfig{
			DNS: []DNSRule{{Servers: []string{"10.0.0.2:53"}}},
		},
	}

	merged := Merge(defaults, &Profile{}, &CLIFlags{})

	require.Len(t, merged.Routing.DNS, 1)
	assert.Equal(t, []string{"10.0.0.2:53"}, merged.Routing.DNS[0].Servers)
}

func TestMerge_DNSProfileReplacesDefaults(t *testing.T) {
	defaults := &Defaults{
		Routing: &RoutingConfig{
			DNS: []DNSRule{
				{Servers: []string{"10.0.0.2:53"}, Patterns: []string{"*.a.example.com"}},
				{Servers: []string{"10.0.0.3:53"}},
			},
		},
	}
	profile := &Profile{
		Routing: &RoutingConfig{
			DNS: []DNSRule{{Servers: []string{"10.1.0.2:53"}}},
		},
	}

	merged := Merge(defaults, profile, &CLIFlags{})

	// Replaced, not appended: a catch-all inherited from defaults would
	// otherwise shadow the profile's rules.
	require.Len(t, merged.Routing.DNS, 1)
	assert.Equal(t, []string{"10.1.0.2:53"}, merged.Routing.DNS[0].Servers)
}

func TestMerge_DNSCLIOverridesServers(t *testing.T) {
	defaults := &Defaults{
		Routing: &RoutingConfig{
			DNS: []DNSRule{{
				Servers:  []string{"10.0.0.2:53"},
				Patterns: []string{"*.internal.example.com"},
			}},
		},
	}
	cli := &CLIFlags{DNSServers: []string{"10.9.9.9:53"}}

	merged := Merge(defaults, &Profile{}, cli)

	require.Len(t, merged.Routing.DNS, 1)
	assert.Equal(t, []string{"10.9.9.9:53"}, merged.Routing.DNS[0].Servers)
	assert.Equal(t, []string{"*.internal.example.com"}, merged.Routing.DNS[0].Patterns,
		"overriding servers must not discard the rule's patterns")
}

func TestMerge_DNSCLICreatesRuleWhenNoneConfigured(t *testing.T) {
	cli := &CLIFlags{DNSServers: []string{"10.9.9.9:53"}}

	merged := Merge(&Defaults{}, &Profile{}, cli)

	require.Len(t, merged.Routing.DNS, 1)
	assert.Equal(t, []string{"10.9.9.9:53"}, merged.Routing.DNS[0].Servers)
	assert.Empty(t, merged.Routing.DNS[0].Patterns, "a bare --dns-server applies to all hosts")
}

func TestMerge_DNSDeepCopyProtectsSource(t *testing.T) {
	defaults := &Defaults{
		Routing: &RoutingConfig{
			DNS: []DNSRule{{Servers: []string{"10.0.0.2:53"}}},
		},
	}

	merged := Merge(defaults, &Profile{}, &CLIFlags{})
	merged.Routing.DNS[0].Servers[0] = "mutated"

	assert.Equal(t, "10.0.0.2:53", defaults.Routing.DNS[0].Servers[0],
		"merging must not alias the source config")
}

func TestMerge_NoDNSLeavesEmpty(t *testing.T) {
	merged := Merge(&Defaults{}, &Profile{}, &CLIFlags{})

	assert.Empty(t, merged.Routing.DNS)
}
