package config

// CLIFlags represents configuration values provided via command-line flags.
// Fields with IsSet suffix indicate whether the flag was explicitly set by the user.
type CLIFlags struct {
	// Instance identification
	InstanceID string
	Name       string

	// AWS settings
	AWSProfile string
	Region     string

	// SSH settings
	SSHKey           string
	SSHUser          string
	SSHKeyPassphrase string

	// Instance lifecycle
	AutoStart      bool
	AutoStartIsSet bool
	AutoStop       bool
	AutoStopIsSet  bool

	// Proxy settings
	Mode       string
	Listen     string
	RemotePort int
	Lazy       *bool
	LazyIsSet  bool

	// Routing settings
	RouteDefault  string
	RouteProxy    []string
	RouteDirect   []string
	RouteVMDirect []string
}

// MergedConfig represents the final merged configuration.
type MergedConfig struct {
	InstanceID       string
	Name             string
	AWSProfile       string
	Region           string
	SSHKey           string
	SSHUser          string
	SSHKeyPassphrase string
	AutoStart        bool
	AutoStop         bool
	Mode             string
	Listen           string
	RemotePort       int
	Lazy             bool
	Routing          *RoutingConfig
}

// Built-in defaults used when no other configuration is provided.
var builtInDefaults = Defaults{
	SSHUser:    "ec2-user",
	Listen:     "127.0.0.1:1080",
	Mode:       "direct",
	RemotePort: 22,
	Lazy:       boolPtr(true),
}

func boolPtr(b bool) *bool {
	return &b
}

// Merge combines configuration from defaults, profile, and CLI flags.
// Priority: CLI > profile > defaults > built-in defaults
func Merge(defaults *Defaults, profile *Profile, cli *CLIFlags) *MergedConfig {
	result := &MergedConfig{
		// Start with built-in defaults
		SSHUser:    builtInDefaults.SSHUser,
		Listen:     builtInDefaults.Listen,
		Mode:       builtInDefaults.Mode,
		RemotePort: builtInDefaults.RemotePort,
		Lazy:       true, // Default lazy to true
		Routing:    &RoutingConfig{},
	}

	// Apply file defaults
	if defaults != nil {
		if defaults.SSHUser != "" {
			result.SSHUser = defaults.SSHUser
		}
		if defaults.Listen != "" {
			result.Listen = defaults.Listen
		}
		if defaults.Mode != "" {
			result.Mode = defaults.Mode
		}
		if defaults.RemotePort != 0 {
			result.RemotePort = defaults.RemotePort
		}
		if defaults.Lazy != nil {
			result.Lazy = *defaults.Lazy
		}
		if defaults.Routing != nil {
			result.Routing = mergeRoutingFromDefaults(defaults.Routing)
		}
	}

	// Apply profile settings
	if profile != nil {
		if profile.InstanceID != "" {
			result.InstanceID = profile.InstanceID
		}
		if profile.Name != "" {
			result.Name = profile.Name
		}
		if profile.AWSProfile != "" {
			result.AWSProfile = profile.AWSProfile
		}
		if profile.Region != "" {
			result.Region = profile.Region
		}
		if profile.SSHKey != "" {
			result.SSHKey = profile.SSHKey
		}
		if profile.SSHUser != "" {
			result.SSHUser = profile.SSHUser
		}
		if profile.SSHKeyPassphrase != "" {
			result.SSHKeyPassphrase = profile.SSHKeyPassphrase
		}
		if profile.AutoStart {
			result.AutoStart = true
		}
		if profile.AutoStop {
			result.AutoStop = true
		}
		if profile.Mode != "" {
			result.Mode = profile.Mode
		}
		if profile.Listen != "" {
			result.Listen = profile.Listen
		}
		if profile.RemotePort != 0 {
			result.RemotePort = profile.RemotePort
		}
		if profile.Lazy != nil {
			result.Lazy = *profile.Lazy
		}
		if profile.Routing != nil {
			result.Routing = mergeRoutingFromProfile(result.Routing, profile.Routing)
		}
	}

	// Apply CLI settings (highest priority)
	if cli != nil {
		if cli.InstanceID != "" {
			result.InstanceID = cli.InstanceID
		}
		if cli.Name != "" {
			result.Name = cli.Name
		}
		if cli.AWSProfile != "" {
			result.AWSProfile = cli.AWSProfile
		}
		if cli.Region != "" {
			result.Region = cli.Region
		}
		if cli.SSHKey != "" {
			result.SSHKey = cli.SSHKey
		}
		if cli.SSHUser != "" {
			result.SSHUser = cli.SSHUser
		}
		if cli.SSHKeyPassphrase != "" {
			result.SSHKeyPassphrase = cli.SSHKeyPassphrase
		}
		if cli.AutoStartIsSet {
			result.AutoStart = cli.AutoStart
		}
		if cli.AutoStopIsSet {
			result.AutoStop = cli.AutoStop
		}
		if cli.Mode != "" {
			result.Mode = cli.Mode
		}
		if cli.Listen != "" {
			result.Listen = cli.Listen
		}
		if cli.RemotePort != 0 {
			result.RemotePort = cli.RemotePort
		}
		if cli.LazyIsSet {
			result.Lazy = *cli.Lazy
		}

		// Apply CLI routing settings
		result.Routing = mergeRoutingFromCLI(result.Routing, cli)
	}

	return result
}

// mergeRoutingFromDefaults creates a new routing config from defaults.
func mergeRoutingFromDefaults(src *RoutingConfig) *RoutingConfig {
	return &RoutingConfig{
		Default:  src.Default,
		Proxy:    copySlice(src.Proxy),
		Direct:   copySlice(src.Direct),
		VMDirect: copySlice(src.VMDirect),
	}
}

// mergeRoutingFromProfile merges profile routing into existing routing config.
func mergeRoutingFromProfile(base *RoutingConfig, profile *RoutingConfig) *RoutingConfig {
	result := &RoutingConfig{
		Default:  base.Default,
		Proxy:    copySlice(base.Proxy),
		Direct:   copySlice(base.Direct),
		VMDirect: copySlice(base.VMDirect),
	}

	if profile.Default != "" {
		result.Default = profile.Default
	}

	// Merge arrays (profile patterns added to existing)
	result.Proxy = mergeStringSlices(result.Proxy, profile.Proxy)
	result.Direct = mergeStringSlices(result.Direct, profile.Direct)
	result.VMDirect = mergeStringSlices(result.VMDirect, profile.VMDirect)

	return result
}

// mergeRoutingFromCLI merges CLI routing settings into existing routing config.
func mergeRoutingFromCLI(base *RoutingConfig, cli *CLIFlags) *RoutingConfig {
	result := &RoutingConfig{
		Default:  base.Default,
		Proxy:    copySlice(base.Proxy),
		Direct:   copySlice(base.Direct),
		VMDirect: copySlice(base.VMDirect),
	}

	if cli.RouteDefault != "" {
		result.Default = cli.RouteDefault
	}

	// Merge arrays (CLI patterns added to existing)
	result.Proxy = mergeStringSlices(result.Proxy, cli.RouteProxy)
	result.Direct = mergeStringSlices(result.Direct, cli.RouteDirect)
	result.VMDirect = mergeStringSlices(result.VMDirect, cli.RouteVMDirect)

	return result
}

// copySlice creates a copy of a string slice.
func copySlice(src []string) []string {
	if src == nil {
		return nil
	}
	result := make([]string, len(src))
	copy(result, src)
	return result
}

// mergeStringSlices merges two slices, avoiding duplicates.
func mergeStringSlices(base, additional []string) []string {
	if len(additional) == 0 {
		return base
	}

	seen := make(map[string]bool)
	result := make([]string, 0, len(base)+len(additional))

	for _, s := range base {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	for _, s := range additional {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}

	return result
}
