// awsocks - AWS SOCKS5 proxy via SSM
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/urfave/cli/v2"
	appconfig "github.com/wadahiro/awsocks/internal/config"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mode"
	"github.com/wadahiro/awsocks/internal/proxy"
	"github.com/wadahiro/awsocks/internal/routing"
	"github.com/wadahiro/awsocks/internal/ui"
)

var version = "dev"

func main() {
	app := &cli.App{
		Name:    "awsocks",
		Usage:   "AWS SOCKS5 proxy via SSM",
		Version: version,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "verbose",
				Usage: "Enable debug logging",
			},
			&cli.StringFlag{
				Name:  "log-level",
				Value: "info",
				Usage: "Log level (debug, info, warn, error)",
			},
		},
		Before: func(c *cli.Context) error {
			// Configure log level (DynamicLevel allows runtime changes)
			if c.Bool("verbose") {
				log.SetVerbose(true)
			} else if level := c.String("log-level"); level != "" {
				log.SetLevel(log.ParseLevel(level))
			}
			return nil
		},
		Commands: []*cli.Command{
			{
				Name:      "start",
				Usage:     "Start the SOCKS5 proxy",
				ArgsUsage: "[profile]",
				Flags: []cli.Flag{
					// Config file
					&cli.StringFlag{
						Name:    "config",
						Aliases: []string{"c"},
						Usage:   "Path to config file",
					},
					// Execution mode
					&cli.StringFlag{
						Name:    "mode",
						Aliases: []string{"m"},
						Usage:   "Execution mode (direct, vm)",
					},
					&cli.StringFlag{
						Name:    "backend",
						Aliases: []string{"b"},
						Value:   "ssm",
						Usage:   "Backend type (ssm)",
					},
					// Instance identification
					&cli.StringFlag{
						Name:    "instance-id",
						Aliases: []string{"i"},
						Usage:   "EC2 instance ID (use --name for Name tag search)",
					},
					&cli.StringFlag{
						Name:    "name",
						Aliases: []string{"n"},
						Usage:   "EC2 Name tag to search (interactive selection if multiple matches)",
					},
					// AWS settings
					&cli.StringFlag{
						Name:    "aws-profile",
						Usage:   "AWS profile name",
						EnvVars: []string{"AWS_PROFILE"},
					},
					&cli.StringFlag{
						Name:    "region",
						Aliases: []string{"r"},
						Usage:   "AWS region",
						EnvVars: []string{"AWS_REGION", "AWS_DEFAULT_REGION"},
					},
					// SSH settings
					&cli.StringFlag{
						Name:    "ssh-user",
						Aliases: []string{"u"},
						Usage:   "SSH username for EC2 instance",
					},
					&cli.StringFlag{
						Name:    "ssh-key",
						Aliases: []string{"k"},
						Usage:   "Path to SSH private key file (auto-detected if not specified)",
					},
					&cli.StringFlag{
						Name:    "ssh-key-passphrase",
						Usage:   "Passphrase for SSH private key",
						EnvVars: []string{"SSH_KEY_PASSPHRASE"},
					},
					// Proxy settings
					&cli.StringFlag{
						Name:    "listen",
						Aliases: []string{"l"},
						Usage:   "SOCKS5 listen address",
					},
					&cli.IntFlag{
						Name:    "remote-port",
						Aliases: []string{"p"},
						Usage:   "Remote port to forward",
					},
					&cli.BoolFlag{
						Name:  "lazy",
						Usage: "Lazy connection mode: connect to EC2/SSM only when first proxy request arrives",
					},
					// Instance lifecycle
					&cli.BoolFlag{
						Name:  "auto-start",
						Usage: "Automatically start EC2 instance if stopped",
					},
					&cli.BoolFlag{
						Name:  "auto-stop",
						Usage: "Automatically stop EC2 instance on exit",
					},
					// Routing settings
					&cli.StringFlag{
						Name:  "route-default",
						Usage: "Default route (proxy, direct, vm-direct)",
					},
					&cli.StringSliceFlag{
						Name:  "route-proxy",
						Usage: "Patterns to route via proxy (can be specified multiple times)",
					},
					&cli.StringSliceFlag{
						Name:  "route-direct",
						Usage: "Patterns to route directly (can be specified multiple times)",
					},
					&cli.StringSliceFlag{
						Name:  "route-vm-direct",
						Usage: "Patterns to route via VM NAT (can be specified multiple times)",
					},
					// Deprecated
					&cli.StringFlag{
						Name:   "routing-config",
						Usage:  "Path to routing config file (deprecated: use config file instead)",
						Hidden: true,
					},
				},
				Action: startAction,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func startAction(c *cli.Context) error {
	// Load config file
	configPath := c.String("config")
	if configPath == "" {
		configPath = appconfig.DefaultConfigPath()
	}

	appCfg, err := appconfig.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get profile name from positional argument
	profileName := c.Args().First()

	// If no profile specified and no instance specified, show interactive selection
	var profile *appconfig.Profile
	if profileName != "" {
		p, exists := appCfg.GetProfile(profileName)
		if !exists {
			return fmt.Errorf("profile '%s' not found in config", profileName)
		}
		profile = &p
	} else if c.String("instance-id") == "" && c.String("name") == "" && len(appCfg.Profiles) > 0 {
		// Interactive profile selection
		profileNames := make([]string, 0, len(appCfg.Profiles))
		for name := range appCfg.Profiles {
			profileNames = append(profileNames, name)
		}
		sort.Strings(profileNames)

		selector := ui.DefaultProfileSelector()
		selected, err := selector.SelectProfile(profileNames)
		if err != nil {
			return fmt.Errorf("profile selection failed: %w", err)
		}
		p := appCfg.Profiles[selected]
		profile = &p
		profileName = selected
	}

	// Build CLI flags
	cli := buildCLIFlags(c)

	// Merge configurations: CLI > profile > defaults > built-in
	merged := appconfig.Merge(&appCfg.Defaults, profile, cli)

	// Auto-detect SSH key if not specified
	if merged.SSHKey == "" && (merged.InstanceID != "" || merged.Name != "") {
		finder := appconfig.DefaultSSHKeyFinder()
		keyPath, err := finder.FindDefaultKey()
		if err != nil {
			return fmt.Errorf("SSH key not found (specify with --ssh-key or add to config): %w", err)
		}
		merged.SSHKey = keyPath
		fmt.Printf("Auto-detected SSH key: %s\n", keyPath)
	}

	// Expand tilde in SSH key path
	merged.SSHKey = expandTilde(merged.SSHKey)

	// Build routing config
	routingCfg := buildRoutingConfig(merged.Routing)

	// Build proxy config
	cfg := &proxy.Config{
		InstanceID:       merged.InstanceID,
		Name:             merged.Name,
		Profile:          merged.AWSProfile,
		Region:           merged.Region,
		ListenAddr:       merged.Listen,
		Mode:             mode.ParseMode(merged.Mode),
		Backend:          c.String("backend"),
		RemotePort:       merged.RemotePort,
		SSHUser:          merged.SSHUser,
		SSHKeyPath:       merged.SSHKey,
		SSHKeyPassphrase: merged.SSHKeyPassphrase,
		AutoStart:        merged.AutoStart,
		AutoStop:         merged.AutoStop,
		RoutingConfig:    routingCfg,
		LazyConnect:      merged.Lazy,
	}

	// Validate mode
	actualMode := mode.SelectMode(cfg.Mode)
	if actualMode == mode.ModeVM && !mode.IsVMSupported() {
		return fmt.Errorf("VM mode is only supported on macOS")
	}

	// Validate backend-specific requirements
	if cfg.Backend == "ssm" && cfg.SSHKeyPath == "" && (cfg.InstanceID != "" || cfg.Name != "") {
		return fmt.Errorf("--ssh-key is required for SSM backend")
	}

	// Print configuration
	fmt.Println("=== awsocks ===")
	fmt.Printf("Mode: %s\n", actualMode)
	fmt.Printf("Backend: %s\n", cfg.Backend)
	fmt.Printf("Listen: %s\n", cfg.ListenAddr)
	if profileName != "" {
		fmt.Printf("Config Profile: %s\n", profileName)
	}
	if cfg.Profile != "" {
		fmt.Printf("AWS Profile: %s\n", cfg.Profile)
	}
	if cfg.Region != "" {
		fmt.Printf("Region: %s\n", cfg.Region)
	}
	if cfg.InstanceID != "" {
		fmt.Printf("Instance: %s\n", cfg.InstanceID)
	}
	if cfg.Name != "" {
		fmt.Printf("Name filter: %s\n", cfg.Name)
	}
	if cfg.Backend == "ssm" && cfg.SSHKeyPath != "" {
		fmt.Printf("SSH User: %s\n", cfg.SSHUser)
		fmt.Printf("SSH Key: %s\n", cfg.SSHKeyPath)
	}
	if routingCfg != nil && routingCfg.Default != "" {
		fmt.Printf("Route default: %s\n", routingCfg.Default)
	}
	if !cfg.LazyConnect {
		fmt.Println("Lazy: disabled")
	}
	fmt.Println()

	// Create proxy manager
	manager, err := proxy.NewManager(cfg)
	if err != nil {
		return fmt.Errorf("failed to create proxy manager: %w", err)
	}

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	shutdownRequested := false
	go func() {
		<-sigCh
		fmt.Println("\n[!] Received signal, shutting down...")
		shutdownRequested = true
		cancel()
		manager.Stop()
	}()

	// Start proxy
	if err := manager.Start(ctx); err != nil {
		// Ignore error if shutdown was requested
		if shutdownRequested {
			return nil
		}
		return fmt.Errorf("proxy error: %w", err)
	}

	return nil
}

func buildCLIFlags(c *cli.Context) *appconfig.CLIFlags {
	cli := &appconfig.CLIFlags{}

	if c.IsSet("instance-id") {
		cli.InstanceID = c.String("instance-id")
	}
	if c.IsSet("name") {
		cli.Name = c.String("name")
	}
	if c.IsSet("aws-profile") {
		cli.AWSProfile = c.String("aws-profile")
	}
	if c.IsSet("region") {
		cli.Region = c.String("region")
	}
	if c.IsSet("ssh-key") {
		cli.SSHKey = c.String("ssh-key")
	}
	if c.IsSet("ssh-user") {
		cli.SSHUser = c.String("ssh-user")
	}
	if c.IsSet("ssh-key-passphrase") {
		cli.SSHKeyPassphrase = c.String("ssh-key-passphrase")
	}
	if c.IsSet("auto-start") {
		cli.AutoStart = c.Bool("auto-start")
		cli.AutoStartIsSet = true
	}
	if c.IsSet("auto-stop") {
		cli.AutoStop = c.Bool("auto-stop")
		cli.AutoStopIsSet = true
	}
	if c.IsSet("mode") {
		cli.Mode = c.String("mode")
	}
	if c.IsSet("listen") {
		cli.Listen = c.String("listen")
	}
	if c.IsSet("remote-port") {
		cli.RemotePort = c.Int("remote-port")
	}
	if c.IsSet("lazy") {
		lazy := c.Bool("lazy")
		cli.Lazy = &lazy
		cli.LazyIsSet = true
	}

	// Routing CLI flags
	if c.IsSet("route-default") {
		cli.RouteDefault = c.String("route-default")
	}
	if c.IsSet("route-proxy") {
		cli.RouteProxy = c.StringSlice("route-proxy")
	}
	if c.IsSet("route-direct") {
		cli.RouteDirect = c.StringSlice("route-direct")
	}
	if c.IsSet("route-vm-direct") {
		cli.RouteVMDirect = c.StringSlice("route-vm-direct")
	}

	return cli
}

func buildRoutingConfig(cfg *appconfig.RoutingConfig) *routing.Config {
	if cfg == nil {
		return nil
	}

	return &routing.Config{
		Default:  cfg.Default,
		Proxy:    cfg.Proxy,
		Direct:   cfg.Direct,
		VMDirect: cfg.VMDirect,
	}
}

func expandTilde(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return strings.Replace(path, "~", home, 1)
}
