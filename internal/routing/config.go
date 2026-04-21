package routing

// Config represents the routing configuration
type Config struct {
	Default  string            `toml:"default"`
	Proxy    []string          `toml:"proxy"`
	Direct   []string          `toml:"direct"`
	VMDirect []string          `toml:"vm-direct"`
	Hosts    map[string]string `toml:"hosts"`
}
