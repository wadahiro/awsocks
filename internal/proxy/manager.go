// Package proxy implements proxy management
package proxy

import (
	"context"
	"fmt"
	golog "log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	gosocks5 "github.com/armon/go-socks5"
	"github.com/wadahiro/awsocks/internal/backend"
	ssmbackend "github.com/wadahiro/awsocks/internal/backend/ssm"
	"github.com/wadahiro/awsocks/internal/credentials"
	ec2pkg "github.com/wadahiro/awsocks/internal/ec2"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mode"
	"github.com/wadahiro/awsocks/internal/protocol"
	"github.com/wadahiro/awsocks/internal/routing"
	"github.com/wadahiro/awsocks/internal/vm"
)

// Config holds proxy configuration
type Config struct {
	InstanceID string
	Name       string
	Profile    string
	Region     string
	ListenAddr string
	Mode       mode.ExecutionMode
	Backend    string
	RemotePort int

	// SSH settings (required for SSM backend)
	SSHUser          string // --ssh-user
	SSHKeyPath       string // --ssh-key
	SSHKeyPassphrase string // --ssh-key-passphrase

	// Auto start/stop settings
	AutoStart bool // --auto-start
	AutoStop  bool // --auto-stop

	// Routing settings
	RoutingConfigPath string // --routing-config

	// Lazy connection settings
	LazyConnect bool // --lazy
}

// Manager manages the proxy lifecycle
type Manager interface {
	Start(ctx context.Context) error
	Stop() error
}

// NewManager creates the appropriate proxy manager based on mode
func NewManager(cfg *Config) (Manager, error) {
	actualMode := mode.SelectMode(cfg.Mode)

	switch actualMode {
	case mode.ModeVM:
		if !mode.IsVMSupported() {
			return nil, fmt.Errorf("VM mode is only supported on macOS")
		}
		return NewVMManager(cfg)
	case mode.ModeDirect:
		return NewDirectManager(cfg)
	default:
		return nil, fmt.Errorf("unknown mode: %v", actualMode)
	}
}

// VMManager uses Virtualization.framework + VM Agent
type VMManager struct {
	cfg                *Config
	vm                 *vm.ProxyVM
	socks5             *SOCKS5Server
	credProv           *credentials.Provider
	agentConn          net.Conn
	ec2Client          ec2pkg.Client
	resolvedInstanceID string
	ctx                context.Context
	cancel             context.CancelFunc

	// Lazy initialization state
	awsInitialized bool
	awsInitMu      sync.Mutex
}

var logger = log.For(log.ComponentManager)

// NewVMManager creates a new VM-based proxy manager
func NewVMManager(cfg *Config) (*VMManager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	return &VMManager{
		cfg:      cfg,
		credProv: credentials.NewProvider(cfg.Profile, cfg.Region),
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

// Start starts the VM-based proxy
func (m *VMManager) Start(ctx context.Context) error {
	logger.Info("Starting VM mode...")

	// For non-lazy mode, initialize AWS immediately
	if !m.cfg.LazyConnect {
		if err := m.initializeAWS(ctx); err != nil {
			return err
		}
	} else {
		// Lazy mode: store instance ID if provided directly (not via name)
		m.resolvedInstanceID = m.cfg.InstanceID
		logger.Info("Lazy connection mode: AWS initialization deferred until first proxy request")
	}

	// Create and start VM
	logger.Info("Creating VM...")
	proxyVM, err := vm.NewProxyVM()
	if err != nil {
		return fmt.Errorf("failed to create VM: %w", err)
	}
	m.vm = proxyVM

	logger.Info("Starting VM...")
	if err := proxyVM.Start(ctx); err != nil {
		proxyVM.Cleanup()
		return fmt.Errorf("failed to start VM: %w", err)
	}

	// Wait for agent to connect
	logger.Info("Waiting for agent to connect via vsock...")
	agentConn, err := proxyVM.WaitForAgent(ctx)
	if err != nil {
		proxyVM.Stop()
		proxyVM.Cleanup()
		return fmt.Errorf("failed to connect to agent: %w", err)
	}
	m.agentConn = agentConn
	logger.Info("Agent connected")

	// Send backend configuration first (before credentials)
	if err := m.sendBackendConfig(); err != nil {
		return fmt.Errorf("failed to send backend config: %w", err)
	}

	// For non-lazy mode, send credentials and start refresh loop
	if !m.cfg.LazyConnect {
		// Send initial credentials
		if err := m.sendCredentials(ctx); err != nil {
			logger.Warn("failed to send initial credentials", "error", err)
		}

		// Start credential refresh goroutine
		go m.credentialRefreshLoop(ctx)
	}

	// Initialize router for VM mode (with vm-direct support)
	var router routing.Router
	if m.cfg.RoutingConfigPath != "" {
		cfg, err := routing.LoadConfig(m.cfg.RoutingConfigPath)
		if err != nil {
			return fmt.Errorf("failed to load routing config: %w", err)
		}
		router = routing.NewRouter(cfg, routing.WithVMMode())
		logger.Info("Routing config loaded", "path", m.cfg.RoutingConfigPath, "default", cfg.Default)
	} else {
		router = routing.NewDefaultRouter(routing.WithVMMode())
	}

	// Start SOCKS5 server (pass VMManager reference for lazy init)
	m.socks5 = NewSOCKS5Server(m.cfg.ListenAddr, agentConn, router)
	if m.cfg.LazyConnect {
		m.socks5.SetVMManager(m)
	}
	logger.Info("Starting SOCKS5 proxy", "listen", m.cfg.ListenAddr)

	return m.socks5.Start()
}

// initializeAWS performs AWS-related initialization (credential provider, config loading, instance resolution)
func (m *VMManager) initializeAWS(ctx context.Context) error {
	// Start credential provider
	if err := m.credProv.Start(ctx); err != nil {
		return fmt.Errorf("failed to start credential provider: %w", err)
	}

	// Load AWS config for EC2 API (instance resolution, auto-start/stop)
	opts := []func(*config.LoadOptions) error{}
	if m.cfg.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(m.cfg.Profile))
	}
	if m.cfg.Region != "" {
		opts = append(opts, config.WithRegion(m.cfg.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	m.ec2Client = ec2pkg.NewClient(awsCfg)

	// Resolve instance ID from Name tag if needed
	if m.cfg.InstanceID == "" && m.cfg.Name != "" {
		resolvedID, state, err := m.resolveInstanceByName(ctx)
		if err != nil {
			return fmt.Errorf("failed to resolve instance: %w", err)
		}
		m.resolvedInstanceID = resolvedID
		logger.Info("Resolved instance", "name", m.cfg.Name, "id", resolvedID, "state", state)

		// Auto-start instance if stopped
		if m.cfg.AutoStart && state == "stopped" {
			logger.Info("Instance is stopped, starting...", "instance", resolvedID)
			instMgr := ec2pkg.NewInstanceManager(m.ec2Client)
			if err := instMgr.StartAndWait(ctx, resolvedID, 5*time.Minute); err != nil {
				return fmt.Errorf("failed to start instance: %w", err)
			}
			logger.Info("Instance is now running", "instance", resolvedID)
		}
	} else {
		m.resolvedInstanceID = m.cfg.InstanceID
	}

	return nil
}

// EnsureInitialized performs lazy AWS initialization on first proxy request
// This is called from SOCKS5Server when LazyConnect is enabled
func (m *VMManager) EnsureInitialized(ctx context.Context) error {
	m.awsInitMu.Lock()
	defer m.awsInitMu.Unlock()

	if m.awsInitialized {
		return nil
	}

	logger.Info("Lazy initialization: starting AWS credential and instance resolution...")

	// 1. Initialize AWS (credential provider, config, instance resolution)
	if err := m.initializeAWS(ctx); err != nil {
		return err
	}

	// 2. Re-send BackendConfig with resolved instance ID
	if err := m.sendBackendConfig(); err != nil {
		return fmt.Errorf("failed to send backend config: %w", err)
	}

	// 3. Send credentials to agent
	if err := m.sendCredentials(ctx); err != nil {
		return fmt.Errorf("failed to send credentials: %w", err)
	}

	// 4. Start credential refresh loop
	go m.credentialRefreshLoop(m.ctx)

	m.awsInitialized = true
	logger.Info("Lazy initialization completed")

	return nil
}

// Stop stops the VM-based proxy
func (m *VMManager) Stop() error {
	m.cancel()

	if m.socks5 != nil {
		m.socks5.Stop()
	}

	if m.agentConn != nil {
		// Send shutdown message
		msg := &protocol.Message{Type: protocol.MsgShutdown}
		protocol.WriteMessage(m.agentConn, msg)
		m.agentConn.Close()
	}

	if m.vm != nil {
		m.vm.Stop()
		m.vm.Cleanup()
	}

	if m.credProv != nil {
		m.credProv.Stop()
	}

	return nil
}

func (m *VMManager) sendBackendConfig() error {
	// Read SSH key file
	keyContent, err := os.ReadFile(m.cfg.SSHKeyPath)
	if err != nil {
		return fmt.Errorf("failed to read SSH key: %w", err)
	}

	// Use resolved instance ID (from --name resolution or direct --instance-id)
	instanceID := m.resolvedInstanceID
	if instanceID == "" {
		instanceID = m.cfg.InstanceID
	}

	cfg := &protocol.BackendConfigPayload{
		Type:             "muxssh",
		InstanceID:       instanceID,
		Region:           m.cfg.Region,
		SSHUser:          m.cfg.SSHUser,
		SSHKeyContent:    keyContent,
		SSHKeyPassphrase: m.cfg.SSHKeyPassphrase,
		LazyConnect:      m.cfg.LazyConnect,
	}

	msg := protocol.NewBackendConfigMessage(cfg)
	if err := protocol.WriteMessage(m.agentConn, msg); err != nil {
		return fmt.Errorf("failed to send backend config: %w", err)
	}

	logger.Info("Backend config sent to agent")
	return nil
}

func (m *VMManager) sendCredentials(ctx context.Context) error {
	creds, err := m.credProv.GetCredentials(ctx)
	if err != nil {
		return err
	}

	msg := protocol.NewCredentialUpdateMessage(protocol.CredentialPayload{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Expiration:      creds.Expires,
	})

	return protocol.WriteMessage(m.agentConn, msg)
}

func (m *VMManager) resolveInstanceByName(ctx context.Context) (string, string, error) {
	resolver := ec2pkg.NewResolver(m.ec2Client)

	instances, err := resolver.ResolveByName(ctx, m.cfg.Name)
	if err != nil {
		return "", "", err
	}

	if len(instances) == 0 {
		return "", "", fmt.Errorf("no instances found with name '%s'", m.cfg.Name)
	}

	if len(instances) == 1 {
		logger.Info("Found instance", "name", instances[0].Name, "id", instances[0].ID, "state", instances[0].State)
		return instances[0].ID, instances[0].State, nil
	}

	// Multiple instances found - use the first one
	logger.Info("Found multiple instances, using first", "count", len(instances), "name", instances[0].Name, "id", instances[0].ID, "state", instances[0].State)
	return instances[0].ID, instances[0].State, nil
}

func (m *VMManager) credentialRefreshLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case creds := <-m.credProv.RefreshChannel():
			logger.Debug("Sending updated credentials to agent...")
			msg := protocol.NewCredentialUpdateMessage(protocol.CredentialPayload{
				AccessKeyID:     creds.AccessKeyID,
				SecretAccessKey: creds.SecretAccessKey,
				SessionToken:    creds.SessionToken,
				Expiration:      creds.Expires,
			})

			if err := protocol.WriteMessage(m.agentConn, msg); err != nil {
				logger.Error("Failed to send credentials", "error", err)
			} else {
				logger.Debug("Credentials sent successfully")
			}
		}
	}
}

// DirectManager runs SSM client directly without VM
type DirectManager struct {
	cfg                *Config
	socks5             *DirectSOCKS5Server
	backend            backend.Backend
	credProv           *credentials.Provider
	ctx                context.Context
	cancel             context.CancelFunc
	resolvedInstanceID string        // resolved instance ID for auto-stop
	ec2Client          ec2pkg.Client // EC2 client for instance management

	// Lazy initialization state
	awsInitialized bool
	awsInitMu      sync.Mutex
}

// NewDirectManager creates a new direct proxy manager
func NewDirectManager(cfg *Config) (*DirectManager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	return &DirectManager{
		cfg:      cfg,
		credProv: credentials.NewProvider(cfg.Profile, cfg.Region),
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

// Start starts the direct proxy
func (m *DirectManager) Start(ctx context.Context) error {
	logger.Info("Starting direct mode (no VM)...")

	// For non-lazy mode, initialize AWS immediately
	if !m.cfg.LazyConnect {
		if err := m.initializeAWS(ctx); err != nil {
			return err
		}
	} else {
		// Lazy mode: store instance ID if provided directly (not via name)
		m.resolvedInstanceID = m.cfg.InstanceID
		logger.Info("Lazy connection mode: AWS initialization deferred until first proxy request")
	}

	// Initialize router
	var router routing.Router
	if m.cfg.RoutingConfigPath != "" {
		cfg, err := routing.LoadConfig(m.cfg.RoutingConfigPath)
		if err != nil {
			return fmt.Errorf("failed to load routing config: %w", err)
		}
		router = routing.NewRouter(cfg)
		logger.Info("Routing config loaded", "path", m.cfg.RoutingConfigPath, "default", cfg.Default)
	} else {
		router = routing.NewDefaultRouter()
	}

	m.socks5 = NewDirectSOCKS5Server(m.cfg, m, router)
	if m.cfg.LazyConnect {
		m.socks5.SetLazyInitializer(m)
	}
	logger.Info("Starting SOCKS5 proxy", "listen", m.cfg.ListenAddr)

	return m.socks5.Start()
}

// initializeAWS performs AWS-related initialization (credential provider, config loading, instance resolution)
func (m *DirectManager) initializeAWS(ctx context.Context) error {
	// Start credential provider
	if err := m.credProv.Start(ctx); err != nil {
		return fmt.Errorf("failed to start credential provider: %w", err)
	}

	// Load AWS config
	opts := []func(*config.LoadOptions) error{}
	if m.cfg.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(m.cfg.Profile))
	}
	if m.cfg.Region != "" {
		opts = append(opts, config.WithRegion(m.cfg.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}
	m.ec2Client = ec2pkg.NewClient(awsCfg)

	// Resolve instance ID from Name tag if needed
	instanceID := m.cfg.InstanceID
	instanceState := ""
	if instanceID == "" && m.cfg.Name != "" {
		resolvedID, state, err := m.resolveInstanceByName(ctx)
		if err != nil {
			return fmt.Errorf("failed to resolve instance: %w", err)
		}
		instanceID = resolvedID
		instanceState = state
		m.resolvedInstanceID = instanceID
	} else if instanceID != "" {
		// Get instance state for directly specified instance ID
		m.resolvedInstanceID = instanceID
		if m.cfg.AutoStart {
			instMgr := ec2pkg.NewInstanceManager(m.ec2Client)
			state, err := instMgr.GetInstanceState(ctx, instanceID)
			if err != nil {
				return fmt.Errorf("failed to get instance state: %w", err)
			}
			instanceState = state
		}
	}

	// Auto-start instance if stopped
	if instanceID != "" && m.cfg.AutoStart && instanceState == "stopped" {
		logger.Info("Instance is stopped, starting...", "instance", instanceID)
		instMgr := ec2pkg.NewInstanceManager(m.ec2Client)
		if err := instMgr.StartAndWait(ctx, instanceID, 5*time.Minute); err != nil {
			return fmt.Errorf("failed to start instance: %w", err)
		}
		logger.Info("Instance is now running", "instance", instanceID)
	}

	// Create and start backend if instance is configured
	if instanceID != "" {
		ssmClient := ssmbackend.NewHTTPClient(awsCfg)

		// Default backend is "ssm" (muxssh)
		backendType := m.cfg.Backend
		if backendType == "" {
			backendType = "ssm"
		}

		if backendType == "ssm" {
			// SSM backend uses MuxSSH (SSH over SSM DataChannel)
			logger.Info("Backend: muxssh", "instance", instanceID)

			backendConfig := &ssmbackend.Config{
				InstanceID: instanceID,
				Region:     m.cfg.Region,
				SSHUser:    m.cfg.SSHUser,
				SSHKeyPath: m.cfg.SSHKeyPath,
			}

			// Pass EC2 client for lazy connection auto-start
			if m.cfg.AutoStart {
				backendConfig.AutoStartEC2 = true
				backendConfig.EC2Client = m.ec2Client
			}

			muxBackend := ssmbackend.New(backendConfig, ssmClient)

			if err := muxBackend.Start(m.ctx); err != nil {
				return fmt.Errorf("failed to start MuxSSH backend: %w", err)
			}
			m.backend = muxBackend

			// Store credentials for backend (will connect on first Dial)
			creds := m.credProv.GetLastCredentials()
			muxBackend.SetCredentials(creds)
		}

		// Start credential refresh loop for backend
		go m.credentialRefreshLoop(m.ctx)
	}

	return nil
}

// EnsureInitialized performs lazy AWS initialization on first proxy request
// This is called from DirectSOCKS5Server when LazyConnect is enabled
func (m *DirectManager) EnsureInitialized(ctx context.Context) error {
	m.awsInitMu.Lock()
	defer m.awsInitMu.Unlock()

	if m.awsInitialized {
		return nil
	}

	logger.Info("Lazy initialization: starting AWS credential and instance resolution...")

	if err := m.initializeAWS(ctx); err != nil {
		return err
	}

	// Update socks5 server's backend reference
	if m.socks5 != nil {
		m.socks5.SetBackend(m.backend)
	}

	m.awsInitialized = true
	logger.Info("Lazy initialization completed")

	return nil
}

// GetBackend returns the current backend (may be nil if not initialized)
func (m *DirectManager) GetBackend() backend.Backend {
	m.awsInitMu.Lock()
	defer m.awsInitMu.Unlock()
	return m.backend
}

// IsInitialized returns true if AWS initialization is complete
func (m *DirectManager) IsInitialized() bool {
	m.awsInitMu.Lock()
	defer m.awsInitMu.Unlock()
	return m.awsInitialized
}

// resolveInstanceByName resolves EC2 instance ID from Name tag
// Returns instance ID and state
// Note: m.ec2Client must be initialized before calling this method
func (m *DirectManager) resolveInstanceByName(ctx context.Context) (string, string, error) {
	resolver := ec2pkg.NewResolver(m.ec2Client)

	// Search for instances
	instances, err := resolver.ResolveByName(ctx, m.cfg.Name)
	if err != nil {
		return "", "", err
	}

	if len(instances) == 1 {
		logger.Info("Found instance", "name", instances[0].Name, "id", instances[0].ID, "state", instances[0].State)
		return instances[0].ID, instances[0].State, nil
	}

	// Multiple instances found - for now just use the first one
	// TODO: Add interactive selection
	logger.Info("Found multiple instances, using first", "count", len(instances), "name", instances[0].Name, "id", instances[0].ID, "state", instances[0].State)
	return instances[0].ID, instances[0].State, nil
}

// credentialRefreshLoop sends updated credentials to the backend
func (m *DirectManager) credentialRefreshLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case creds := <-m.credProv.RefreshChannel():
			if m.backend == nil {
				// Backend not yet initialized, skip this update
				continue
			}
			logger.Debug("Sending updated credentials to backend...")
			if err := m.backend.OnCredentialUpdate(creds); err != nil {
				logger.Error("Failed to update backend credentials", "error", err)
			} else {
				logger.Debug("Backend credentials updated successfully")
			}
		}
	}
}

// Stop stops the direct proxy
func (m *DirectManager) Stop() error {
	m.cancel()

	if m.socks5 != nil {
		m.socks5.Stop()
	}

	if m.backend != nil {
		m.backend.Close()
	}

	if m.credProv != nil {
		m.credProv.Stop()
	}

	// Auto-stop instance if configured
	if m.cfg.AutoStop && m.resolvedInstanceID != "" && m.ec2Client != nil {
		logger.Info("Auto-stopping instance...", "instance", m.resolvedInstanceID)
		instMgr := ec2pkg.NewInstanceManager(m.ec2Client)
		if err := instMgr.Stop(context.Background(), m.resolvedInstanceID); err != nil {
			logger.Warn("failed to stop instance", "error", err)
		} else {
			logger.Info("Instance stop initiated", "instance", m.resolvedInstanceID)
		}
	}

	return nil
}

// Dialer is an interface for dialing connections (subset of backend.Backend)
type Dialer interface {
	Dial(ctx context.Context, network, address string) (net.Conn, error)
}

// DirectSOCKS5Server provides SOCKS5 proxy with direct network access
type DirectSOCKS5Server struct {
	cfg             *Config
	backend         backend.Backend
	dialer          Dialer // For testing without full backend
	backendMu       sync.RWMutex
	router          routing.Router
	ctx             context.Context
	cancel          context.CancelFunc
	listener        net.Listener
	listenerMu      sync.Mutex
	lazyInitializer LazyInitializer
}

// NewDirectSOCKS5Server creates a new direct SOCKS5 server
func NewDirectSOCKS5Server(cfg *Config, manager *DirectManager, router routing.Router) *DirectSOCKS5Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &DirectSOCKS5Server{
		cfg:    cfg,
		router: router,
		ctx:    ctx,
		cancel: cancel,
	}
}

// SetLazyInitializer sets the lazy initializer for deferred AWS initialization
func (s *DirectSOCKS5Server) SetLazyInitializer(initializer LazyInitializer) {
	s.lazyInitializer = initializer
}

// SetBackend updates the backend after lazy initialization
func (s *DirectSOCKS5Server) SetBackend(b backend.Backend) {
	s.backendMu.Lock()
	s.backend = b
	s.backendMu.Unlock()
}

// GetBackend returns the current backend (thread-safe)
func (s *DirectSOCKS5Server) GetBackend() backend.Backend {
	s.backendMu.RLock()
	defer s.backendMu.RUnlock()
	return s.backend
}

// SetBackendDialer sets a simple dialer for testing
func (s *DirectSOCKS5Server) SetBackendDialer(d Dialer) {
	s.backendMu.Lock()
	s.dialer = d
	s.backendMu.Unlock()
}

// GetDialer returns the current dialer (thread-safe)
func (s *DirectSOCKS5Server) GetDialer() Dialer {
	s.backendMu.RLock()
	defer s.backendMu.RUnlock()
	if s.dialer != nil {
		return s.dialer
	}
	return s.backend
}

// IsInitialized returns true if lazy initialization is complete
func (s *DirectSOCKS5Server) IsInitialized() bool {
	s.backendMu.RLock()
	defer s.backendMu.RUnlock()
	// If there's a backend, initialization is complete
	// If there's no lazy initializer, we're already initialized
	return s.backend != nil || s.lazyInitializer == nil
}

// Start starts the direct SOCKS5 server
func (s *DirectSOCKS5Server) Start() error {
	dialer := &directDialer{
		cfg:    s.cfg,
		server: s,
		router: s.router,
	}

	conf := &gosocks5.Config{
		Dial:     dialer.Dial,
		Resolver: &noopResolver{},
		Logger:   golog.New(&slogWriter{}, "", 0),
	}

	server, err := gosocks5.New(conf)
	if err != nil {
		return fmt.Errorf("failed to create SOCKS5 server: %w", err)
	}

	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.cfg.ListenAddr, err)
	}

	s.listenerMu.Lock()
	s.listener = listener
	s.listenerMu.Unlock()

	return server.Serve(listener)
}

// Stop stops the direct SOCKS5 server
func (s *DirectSOCKS5Server) Stop() {
	s.cancel()
	s.listenerMu.Lock()
	if s.listener != nil {
		s.listener.Close()
	}
	s.listenerMu.Unlock()
}

type directDialer struct {
	cfg    *Config
	server *DirectSOCKS5Server
	router routing.Router
}

// noopResolver is a NameResolver that does not resolve hostnames
// It returns a nil IP so that the Dial function receives the original hostname
type noopResolver struct{}

func (r *noopResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	// Return nil IP to indicate no resolution was done
	// The dialer will receive the original hostname
	return ctx, nil, nil
}

func (d *directDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	// Check if lazy initialization is in progress or not yet started
	// If so, use direct connection to avoid blocking (e.g., OIDC auth flow)
	if d.server.lazyInitializer != nil && !d.server.IsInitialized() {
		// Try non-blocking initialization (will start if not already running)
		go d.server.lazyInitializer.EnsureInitialized(context.Background())

		// Use direct connection while initializing
		logger.Debug("Initialization in progress, using direct connection", "address", addr)
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	}

	// Determine route
	route := routing.RouteProxy
	if d.router != nil {
		route = d.router.Route(host)
	}

	// For direct route, connect directly
	if route == routing.RouteDirect {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	}

	// Try primary route (proxy)
	conn, err := d.dialWithRoute(ctx, network, addr, route)
	if err == nil {
		return conn, nil
	}

	// Check if fallback is needed
	if !routing.IsFallbackableError(err) {
		return nil, err
	}

	// Get fallback route
	fallbackRoute := d.router.FallbackRoute(route)
	if fallbackRoute == "" {
		return nil, err // No fallback available
	}

	logger.Info("Fallback to alternative route",
		"address", addr, "from", route, "to", fallbackRoute, "reason", err)

	return d.dialWithRoute(ctx, network, addr, fallbackRoute)
}

func (d *directDialer) dialWithRoute(ctx context.Context, network, addr string, route routing.Route) (net.Conn, error) {
	switch route {
	case routing.RouteDirect:
		// Direct connection from host
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	default:
		// RouteProxy: use dialer if available (backend or test dialer)
		dialer := d.server.GetDialer()
		if dialer != nil {
			return dialer.Dial(ctx, network, addr)
		}
		// Fall back to direct connection
		var netDialer net.Dialer
		return netDialer.DialContext(ctx, network, addr)
	}
}

// Ensure interfaces are implemented
var _ Manager = (*VMManager)(nil)
var _ Manager = (*DirectManager)(nil)
