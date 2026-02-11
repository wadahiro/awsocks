package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_ValidTOML(t *testing.T) {
	content := `
[defaults]
ssh-user = "ec2-user"
listen = "127.0.0.1:1080"
lazy = true

[defaults.routing]
default = "proxy"
direct = ["localhost", "127.0.0.1"]

[profiles.work]
name = "bastion-prod"
aws-profile = "work-sso"
region = "ap-northeast-1"
ssh-key = "~/.ssh/work_key"
auto-start = true

[profiles.work.routing]
proxy = ["*.companyA.internal"]

[profiles.client]
name = "client-bastion"
aws-profile = "client-sso"
region = "us-east-1"
ssh-key = "~/.ssh/client_key"

[profiles.client.routing]
proxy = ["*.companyB.internal"]
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0644))

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	// Verify defaults
	assert.Equal(t, "ec2-user", cfg.Defaults.SSHUser)
	assert.Equal(t, "127.0.0.1:1080", cfg.Defaults.Listen)
	require.NotNil(t, cfg.Defaults.Lazy)
	assert.True(t, *cfg.Defaults.Lazy)
	require.NotNil(t, cfg.Defaults.Routing)
	assert.Equal(t, "proxy", cfg.Defaults.Routing.Default)
	assert.Equal(t, []string{"localhost", "127.0.0.1"}, cfg.Defaults.Routing.Direct)

	// Verify profiles
	assert.Len(t, cfg.Profiles, 2)

	work := cfg.Profiles["work"]
	assert.Equal(t, "bastion-prod", work.Name)
	assert.Equal(t, "work-sso", work.AWSProfile)
	assert.Equal(t, "ap-northeast-1", work.Region)
	assert.Equal(t, "~/.ssh/work_key", work.SSHKey)
	assert.True(t, work.AutoStart)
	require.NotNil(t, work.Routing)
	assert.Equal(t, []string{"*.companyA.internal"}, work.Routing.Proxy)

	client := cfg.Profiles["client"]
	assert.Equal(t, "client-bastion", client.Name)
	assert.Equal(t, "client-sso", client.AWSProfile)
	assert.Equal(t, "us-east-1", client.Region)
}

func TestLoadConfig_FileNotFound_ReturnsEmpty(t *testing.T) {
	cfg, err := LoadConfig("/nonexistent/path/config.toml")
	require.NoError(t, err)
	assert.NotNil(t, cfg)
	assert.Empty(t, cfg.Profiles)
}

func TestLoadConfig_InvalidTOML_ReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("invalid toml [[["), 0644))

	_, err := LoadConfig(configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestLoadConfig_ExpandsTilde(t *testing.T) {
	// Create config in home directory
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	configDir := filepath.Join(home, ".config", "awsocks-test")
	require.NoError(t, os.MkdirAll(configDir, 0755))
	defer os.RemoveAll(configDir)

	configPath := filepath.Join(configDir, "config.toml")
	content := `[profiles.test]
name = "test-instance"
`
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0644))

	// Load with tilde path
	cfg, err := LoadConfig("~/.config/awsocks-test/config.toml")
	require.NoError(t, err)
	assert.Contains(t, cfg.Profiles, "test")
}

func TestGetProfile_Exists(t *testing.T) {
	cfg := &AppConfig{
		Profiles: map[string]Profile{
			"work": {
				Name:       "bastion",
				AWSProfile: "work-sso",
			},
		},
	}

	profile, exists := cfg.GetProfile("work")
	assert.True(t, exists)
	assert.Equal(t, "bastion", profile.Name)
	assert.Equal(t, "work-sso", profile.AWSProfile)
}

func TestGetProfile_NotFound(t *testing.T) {
	cfg := &AppConfig{
		Profiles: map[string]Profile{},
	}

	_, exists := cfg.GetProfile("nonexistent")
	assert.False(t, exists)
}

func TestDefaultConfigPath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	expected := filepath.Join(home, ".config", "awsocks", "config.toml")
	assert.Equal(t, expected, DefaultConfigPath())
}

func TestLoadConfig_AllProfileFields(t *testing.T) {
	content := `
[profiles.full]
instance-id = "i-1234567890abcdef0"
name = "bastion"
aws-profile = "my-sso"
region = "us-west-2"
ssh-key = "~/.ssh/my_key"
ssh-user = "ubuntu"
ssh-key-passphrase = "secret"
auto-start = true
auto-stop = true
mode = "direct"
listen = "0.0.0.0:8080"
remote-port = 2222
lazy = false
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0644))

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	full := cfg.Profiles["full"]
	assert.Equal(t, "i-1234567890abcdef0", full.InstanceID)
	assert.Equal(t, "bastion", full.Name)
	assert.Equal(t, "my-sso", full.AWSProfile)
	assert.Equal(t, "us-west-2", full.Region)
	assert.Equal(t, "~/.ssh/my_key", full.SSHKey)
	assert.Equal(t, "ubuntu", full.SSHUser)
	assert.Equal(t, "secret", full.SSHKeyPassphrase)
	assert.True(t, full.AutoStart)
	assert.True(t, full.AutoStop)
	assert.Equal(t, "direct", full.Mode)
	assert.Equal(t, "0.0.0.0:8080", full.Listen)
	assert.Equal(t, 2222, full.RemotePort)
	require.NotNil(t, full.Lazy)
	assert.False(t, *full.Lazy)
}

func TestLoadConfig_RoutingConfig(t *testing.T) {
	content := `
[defaults.routing]
default = "direct"
proxy = ["*.internal", "10.*"]
direct = ["localhost"]
vm-direct = ["*.local"]

[profiles.work.routing]
default = "proxy"
proxy = ["*.company.com"]
direct = ["127.0.0.1"]
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0644))

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	// Defaults routing
	require.NotNil(t, cfg.Defaults.Routing)
	assert.Equal(t, "direct", cfg.Defaults.Routing.Default)
	assert.Equal(t, []string{"*.internal", "10.*"}, cfg.Defaults.Routing.Proxy)
	assert.Equal(t, []string{"localhost"}, cfg.Defaults.Routing.Direct)
	assert.Equal(t, []string{"*.local"}, cfg.Defaults.Routing.VMDirect)

	// Profile routing
	work := cfg.Profiles["work"]
	require.NotNil(t, work.Routing)
	assert.Equal(t, "proxy", work.Routing.Default)
	assert.Equal(t, []string{"*.company.com"}, work.Routing.Proxy)
	assert.Equal(t, []string{"127.0.0.1"}, work.Routing.Direct)
}
