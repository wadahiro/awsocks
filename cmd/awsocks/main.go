// awsocks - AWS SOCKS5 proxy via SSM
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mode"
	"github.com/wadahiro/awsocks/internal/proxy"
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
				Name:  "start",
				Usage: "Start the SOCKS5 proxy",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "mode",
						Aliases: []string{"m"},
						Value:   "direct",
						Usage:   "Execution mode (direct, vm)",
					},
					&cli.StringFlag{
						Name:    "backend",
						Aliases: []string{"b"},
						Value:   "ssm",
						Usage:   "Backend type (ssm)",
					},
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
					&cli.IntFlag{
						Name:    "remote-port",
						Aliases: []string{"p"},
						Value:   22,
						Usage:   "Remote port to forward",
					},
					&cli.StringFlag{
						Name:    "profile",
						Usage:   "AWS profile name",
						EnvVars: []string{"AWS_PROFILE"},
					},
					&cli.StringFlag{
						Name:    "region",
						Aliases: []string{"r"},
						Usage:   "AWS region",
						EnvVars: []string{"AWS_REGION", "AWS_DEFAULT_REGION"},
					},
					&cli.StringFlag{
						Name:    "listen",
						Aliases: []string{"l"},
						Usage:   "SOCKS5 listen address",
						Value:   "127.0.0.1:1080",
					},
					&cli.BoolFlag{
						Name:  "auto-start",
						Usage: "Automatically start EC2 instance if stopped",
					},
					&cli.BoolFlag{
						Name:  "auto-stop",
						Usage: "Automatically stop EC2 instance on exit",
					},
					&cli.StringFlag{
						Name:    "ssh-user",
						Aliases: []string{"u"},
						Value:   "ec2-user",
						Usage:   "SSH username for EC2 instance",
					},
					&cli.StringFlag{
						Name:    "ssh-key",
						Aliases: []string{"k"},
						Usage:   "Path to SSH private key file",
					},
					&cli.StringFlag{
						Name:    "ssh-key-passphrase",
						Usage:   "Passphrase for SSH private key",
						EnvVars: []string{"SSH_KEY_PASSPHRASE"},
					},
					&cli.StringFlag{
						Name:  "routing-config",
						Usage: "Path to routing config file (default: ~/.config/awsocks/config.toml)",
					},
					&cli.BoolFlag{
						Name:  "lazy",
						Value: true,
						Usage: "Lazy connection mode: connect to EC2/SSM only when first proxy request arrives (default: true)",
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
	cfg := &proxy.Config{
		InstanceID:        c.String("instance-id"),
		Name:              c.String("name"),
		Profile:           c.String("profile"),
		Region:            c.String("region"),
		ListenAddr:        c.String("listen"),
		Mode:              mode.ParseMode(c.String("mode")),
		Backend:           c.String("backend"),
		RemotePort:        c.Int("remote-port"),
		SSHUser:           c.String("ssh-user"),
		SSHKeyPath:        c.String("ssh-key"),
		SSHKeyPassphrase:  c.String("ssh-key-passphrase"),
		AutoStart:         c.Bool("auto-start"),
		AutoStop:          c.Bool("auto-stop"),
		RoutingConfigPath: c.String("routing-config"),
		LazyConnect:       c.Bool("lazy"),
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

	fmt.Println("=== awsocks ===")
	fmt.Printf("Mode: %s\n", actualMode)
	fmt.Printf("Backend: %s\n", cfg.Backend)
	fmt.Printf("Listen: %s\n", cfg.ListenAddr)
	if cfg.Profile != "" {
		fmt.Printf("Profile: %s\n", cfg.Profile)
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
	if cfg.RoutingConfigPath != "" {
		fmt.Printf("Routing: %s\n", cfg.RoutingConfigPath)
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
