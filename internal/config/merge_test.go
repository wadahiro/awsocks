package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMerge_CLIOverridesProfile(t *testing.T) {
	defaults := &Defaults{
		SSHUser: "ec2-user",
		Listen:  "127.0.0.1:1080",
	}
	profile := &Profile{
		Name:       "bastion-prod",
		AWSProfile: "work-sso",
		Region:     "ap-northeast-1",
		SSHKey:     "~/.ssh/work_key",
		SSHUser:    "profile-user",
		Listen:     "127.0.0.1:2080",
	}
	cli := &CLIFlags{
		Region:  "us-west-2",
		SSHUser: "cli-user",
		Listen:  "0.0.0.0:3080",
	}

	result := Merge(defaults, profile, cli)

	// CLI values should override profile
	assert.Equal(t, "us-west-2", result.Region)
	assert.Equal(t, "cli-user", result.SSHUser)
	assert.Equal(t, "0.0.0.0:3080", result.Listen)

	// Profile values should be preserved where CLI not set
	assert.Equal(t, "bastion-prod", result.Name)
	assert.Equal(t, "work-sso", result.AWSProfile)
	assert.Equal(t, "~/.ssh/work_key", result.SSHKey)
}

func TestMerge_ProfileOverridesDefaults(t *testing.T) {
	defaults := &Defaults{
		SSHUser:    "ec2-user",
		Listen:     "127.0.0.1:1080",
		RemotePort: 22,
		Lazy:       boolPtr(true),
	}
	profile := &Profile{
		Name:       "bastion",
		SSHUser:    "ubuntu",
		RemotePort: 2222,
	}
	cli := &CLIFlags{}

	result := Merge(defaults, profile, cli)

	// Profile values override defaults
	assert.Equal(t, "ubuntu", result.SSHUser)
	assert.Equal(t, 2222, result.RemotePort)

	// Defaults apply where profile not set
	assert.Equal(t, "127.0.0.1:1080", result.Listen)
	assert.True(t, result.Lazy)
}

func TestMerge_DefaultsApplied(t *testing.T) {
	defaults := &Defaults{
		SSHUser:    "ec2-user",
		Listen:     "127.0.0.1:1080",
		Mode:       "direct",
		RemotePort: 22,
		Lazy:       boolPtr(true),
	}
	profile := &Profile{
		Name: "bastion",
	}
	cli := &CLIFlags{}

	result := Merge(defaults, profile, cli)

	assert.Equal(t, "ec2-user", result.SSHUser)
	assert.Equal(t, "127.0.0.1:1080", result.Listen)
	assert.Equal(t, "direct", result.Mode)
	assert.Equal(t, 22, result.RemotePort)
	assert.True(t, result.Lazy)
}

func TestMerge_BuiltInDefaults(t *testing.T) {
	// No config, no profile, no CLI - should use built-in defaults
	result := Merge(nil, nil, &CLIFlags{})

	assert.Equal(t, "ec2-user", result.SSHUser)
	assert.Equal(t, "127.0.0.1:1080", result.Listen)
	assert.Equal(t, "direct", result.Mode)
	assert.Equal(t, 22, result.RemotePort)
	assert.True(t, result.Lazy)
}

func TestMerge_RoutingPriority(t *testing.T) {
	defaults := &Defaults{
		Routing: &RoutingConfig{
			Default: "proxy",
			Direct:  []string{"localhost"},
		},
	}
	profile := &Profile{
		Routing: &RoutingConfig{
			Default: "direct",
			Proxy:   []string{"*.internal"},
		},
	}
	cli := &CLIFlags{
		RouteDefault: "proxy",
	}

	result := Merge(defaults, profile, cli)

	// CLI route default overrides profile
	assert.Equal(t, "proxy", result.Routing.Default)

	// Profile proxy patterns preserved
	assert.Equal(t, []string{"*.internal"}, result.Routing.Proxy)
}

func TestMerge_RoutingArraysMerged(t *testing.T) {
	defaults := &Defaults{
		Routing: &RoutingConfig{
			Default: "proxy",
			Direct:  []string{"localhost", "127.0.0.1"},
			Proxy:   []string{"*.default.internal"},
		},
	}
	profile := &Profile{
		Routing: &RoutingConfig{
			Direct: []string{"192.168.*"},
			Proxy:  []string{"*.profile.internal"},
		},
	}
	cli := &CLIFlags{
		RouteDirect: []string{"10.*"},
		RouteProxy:  []string{"*.cli.internal"},
	}

	result := Merge(defaults, profile, cli)

	// Arrays should be merged in priority order: CLI, profile, defaults
	assert.Contains(t, result.Routing.Direct, "localhost")
	assert.Contains(t, result.Routing.Direct, "127.0.0.1")
	assert.Contains(t, result.Routing.Direct, "192.168.*")
	assert.Contains(t, result.Routing.Direct, "10.*")

	assert.Contains(t, result.Routing.Proxy, "*.default.internal")
	assert.Contains(t, result.Routing.Proxy, "*.profile.internal")
	assert.Contains(t, result.Routing.Proxy, "*.cli.internal")
}

func TestMerge_LazyBoolOverrides(t *testing.T) {
	tests := []struct {
		name          string
		defaultsLazy  *bool
		profileLazy   *bool
		cliLazy       *bool
		cliLazyIsSet  bool
		expectedLazy  bool
	}{
		{
			name:         "CLI true overrides all",
			defaultsLazy: boolPtr(false),
			profileLazy:  boolPtr(false),
			cliLazy:      boolPtr(true),
			cliLazyIsSet: true,
			expectedLazy: true,
		},
		{
			name:         "CLI false overrides all",
			defaultsLazy: boolPtr(true),
			profileLazy:  boolPtr(true),
			cliLazy:      boolPtr(false),
			cliLazyIsSet: true,
			expectedLazy: false,
		},
		{
			name:         "Profile overrides defaults when CLI not set",
			defaultsLazy: boolPtr(true),
			profileLazy:  boolPtr(false),
			cliLazy:      nil,
			cliLazyIsSet: false,
			expectedLazy: false,
		},
		{
			name:         "Defaults used when profile nil",
			defaultsLazy: boolPtr(true),
			profileLazy:  nil,
			cliLazy:      nil,
			cliLazyIsSet: false,
			expectedLazy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defaults := &Defaults{Lazy: tt.defaultsLazy}
			profile := &Profile{Lazy: tt.profileLazy}
			cli := &CLIFlags{Lazy: tt.cliLazy, LazyIsSet: tt.cliLazyIsSet}

			result := Merge(defaults, profile, cli)
			assert.Equal(t, tt.expectedLazy, result.Lazy)
		})
	}
}

func TestMerge_VMDirectRouting(t *testing.T) {
	defaults := &Defaults{
		Routing: &RoutingConfig{
			VMDirect: []string{"*.vm.local"},
		},
	}
	profile := &Profile{
		Routing: &RoutingConfig{
			VMDirect: []string{"*.profile.vm"},
		},
	}
	cli := &CLIFlags{
		RouteVMDirect: []string{"*.cli.vm"},
	}

	result := Merge(defaults, profile, cli)

	assert.Contains(t, result.Routing.VMDirect, "*.vm.local")
	assert.Contains(t, result.Routing.VMDirect, "*.profile.vm")
	assert.Contains(t, result.Routing.VMDirect, "*.cli.vm")
}

func TestMerge_InstanceIDAndName(t *testing.T) {
	profile := &Profile{
		InstanceID: "i-profile",
		Name:       "profile-name",
	}
	cli := &CLIFlags{
		InstanceID: "i-cli",
		Name:       "cli-name",
	}

	result := Merge(nil, profile, cli)

	// CLI should override
	assert.Equal(t, "i-cli", result.InstanceID)
	assert.Equal(t, "cli-name", result.Name)
}

func TestMerge_AutoStartStop(t *testing.T) {
	profile := &Profile{
		AutoStart: true,
		AutoStop:  false,
	}
	cli := &CLIFlags{
		AutoStop:      true,
		AutoStopIsSet: true,
	}

	result := Merge(nil, profile, cli)

	// Profile AutoStart preserved, CLI AutoStop overrides
	assert.True(t, result.AutoStart)
	assert.True(t, result.AutoStop)
}

func TestMerge_SSHKeyPassphrase(t *testing.T) {
	profile := &Profile{
		SSHKeyPassphrase: "profile-pass",
	}
	cli := &CLIFlags{
		SSHKeyPassphrase: "cli-pass",
	}

	result := Merge(nil, profile, cli)

	assert.Equal(t, "cli-pass", result.SSHKeyPassphrase)
}
