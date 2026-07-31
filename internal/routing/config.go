package routing

// Config represents the routing configuration
type Config struct {
	Default  string            `toml:"default"`
	Proxy    []string          `toml:"proxy"`
	Direct   []string          `toml:"direct"`
	VMDirect []string          `toml:"vm-direct"`
	Hosts    map[string]string `toml:"hosts"`

	// PreConnectDirect lists hosts to route directly (bypassing proxy)
	// only while the proxy backend has not finished connecting yet.
	// Once the backend is active, these hosts follow the normal Route() decision.
	PreConnectDirect []string `toml:"pre-connect-direct"`
	// PreConnectVMDirect is the vm-direct equivalent of PreConnectDirect.
	PreConnectVMDirect []string `toml:"pre-connect-vm-direct"`
}
