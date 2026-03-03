// Package proxy implements proxy management
package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/wadahiro/awsocks/internal/awsapi"
	"github.com/wadahiro/awsocks/internal/backend"
	"github.com/wadahiro/awsocks/internal/clock"
	"github.com/wadahiro/awsocks/internal/credentials"
	ec2pkg "github.com/wadahiro/awsocks/internal/ec2"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/mux"
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
	RoutingConfig *routing.Config

	// Lazy connection settings
	LazyConnect bool // --lazy

	// Idle timeout settings
	IdleTimeout time.Duration

	// SSH keepalive interval (0 to disable)
	SSHKeepaliveInterval time.Duration

	// Proxy network: "direct" (default) or "vm"
	ProxyNetwork string
}

// Dialer is an interface for dialing connections (subset of backend.Backend)
type Dialer interface {
	Dial(ctx context.Context, network, address string) (net.Conn, error)
}

var logger = log.For(log.ComponentManager)

// Manager manages the proxy lifecycle
type Manager struct {
	cfg                *Config
	vm                 *vm.ProxyVM         // only when needsVM
	agentMux           *mux.AgentMux       // only when needsVM (shared multiplexer)
	awsClient          *awsapi.Client      // only when needsProxy
	backend            backend.Backend     // only when needsProxy
	credProv           *credentials.Provider
	socks5             *SOCKS5Server
	router             routing.Router
	idleTracker        *IdleTracker
	clock              clock.Clock
	resolvedInstanceID string
	ec2Client          ec2pkg.Client
	ctx                context.Context
	cancel             context.CancelFunc

	// Lazy initialization state
	awsInitialized   bool
	awsInitMu        sync.Mutex
	initDone         chan struct{}
	initErr          error
	initializeProxyFn func(ctx context.Context) error // override for testing
}

// NewManager creates a new proxy manager
func NewManager(cfg *Config) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		cfg:      cfg,
		credProv: credentials.NewProvider(cfg.Profile, cfg.Region),
		ctx:      ctx,
		cancel:   cancel,
		initDone: make(chan struct{}),
		clock:    clock.RealClock{},
	}, nil
}

// Start starts the proxy
func (m *Manager) Start(ctx context.Context) error {
	// 1. Create router
	m.router = m.createRouter()

	// Determine what we need
	needsVM := m.needsVM()
	needsProxy := m.needsProxy()

	if needsVM {
		logger.Info("VM required (vm-direct routes or proxy-network=vm)")
	}
	if needsProxy {
		logger.Info("Proxy required (instance configured)")
	}
	if !needsVM && !needsProxy {
		logger.Info("Direct-only mode (no VM, no proxy)")
	}

	// 2. Setup idle tracker
	if m.cfg.IdleTimeout > 0 && needsProxy {
		m.idleTracker = NewIdleTracker(m.cfg.IdleTimeout, m.clock, func() {
			m.suspend()
		})
		logger.Info("Idle timeout configured", "timeout", m.cfg.IdleTimeout)
	}

	// 3. Start VM if needed
	if needsVM {
		if err := m.startVM(ctx); err != nil {
			return err
		}
	}

	// 4. Initialize proxy if needed and not lazy
	if needsProxy && !m.cfg.LazyConnect {
		if err := m.initializeProxy(ctx); err != nil {
			return err
		}
		close(m.initDone)

		if m.idleTracker != nil {
			m.idleTracker.Start()
		}
	} else if needsProxy {
		m.resolvedInstanceID = m.cfg.InstanceID
		logger.Info("Lazy connection mode: AWS initialization deferred until first proxy request")
	}

	// 5. Create and start SOCKS5 server
	m.socks5 = NewSOCKS5Server(m.cfg, m.router, m.agentMux)

	if m.backend != nil {
		m.socks5.SetBackend(m.backend)
	}

	if needsProxy && (m.cfg.LazyConnect || m.cfg.IdleTimeout > 0) {
		m.socks5.SetLazyInitializer(m)
	}

	if m.idleTracker != nil {
		m.socks5.SetIdleTracker(m.idleTracker)
	}

	logger.Info("Starting SOCKS5 proxy", "listen", m.cfg.ListenAddr)
	return m.socks5.Start()
}

// needsVM determines if a VM should be started
func (m *Manager) needsVM() bool {
	// VM needed if proxy-network=vm
	if m.cfg.ProxyNetwork == "vm" {
		return true
	}

	// VM needed if any vm-direct routes are configured
	if r, ok := m.router.(*routing.DefaultRouter); ok {
		return r.HasVMDirectRoutes()
	}

	return false
}

// needsProxy determines if proxy (SSM backend) is needed
func (m *Manager) needsProxy() bool {
	return m.cfg.InstanceID != "" || m.cfg.Name != ""
}

// createRouter creates the appropriate router
func (m *Manager) createRouter() routing.Router {
	// VM mode is determined by config, not by old mode flag
	hasVMDirect := false
	if m.cfg.RoutingConfig != nil && len(m.cfg.RoutingConfig.VMDirect) > 0 {
		hasVMDirect = true
	}
	needsVM := m.cfg.ProxyNetwork == "vm" || hasVMDirect

	var opts []routing.RouterOption
	if needsVM {
		opts = append(opts, routing.WithVMMode())
	}

	if m.cfg.RoutingConfig != nil {
		router := routing.NewRouter(m.cfg.RoutingConfig, opts...)
		logger.Info("Routing config loaded", "default", m.cfg.RoutingConfig.Default)
		return router
	}

	return routing.NewDefaultRouter(opts...)
}

// startVM creates and starts the VM, waits for agent connection
func (m *Manager) startVM(ctx context.Context) error {
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

	logger.Info("Waiting for agent to connect via vsock...")
	agentConn, err := proxyVM.WaitForAgent(ctx)
	if err != nil {
		proxyVM.Stop()
		proxyVM.Cleanup()
		return fmt.Errorf("failed to connect to agent: %w", err)
	}
	// Create shared multiplexer with log handler
	m.agentMux = mux.NewAgentMux(agentConn, mux.WithLogHandler(handleAgentLog))
	logger.Info("Agent connected")

	return nil
}

// handleAgentLog processes log messages forwarded from the VM agent.
func handleAgentLog(payload *protocol.LogPayload) {
	agentLogger := log.For(log.ComponentAgent)
	switch payload.Level {
	case "debug":
		agentLogger.Debug(payload.Message)
	case "info":
		agentLogger.Info(payload.Message)
	case "warn":
		agentLogger.Warn(payload.Message)
	case "error":
		agentLogger.Error(payload.Message)
	default:
		agentLogger.Info(payload.Message)
	}
}

// initializeProxy performs AWS-related initialization
func (m *Manager) initializeProxy(ctx context.Context) error {
	// Start credential provider
	if err := m.credProv.Start(ctx); err != nil {
		return fmt.Errorf("failed to start credential provider: %w", err)
	}

	awsCfg := m.credProv.GetConfig()

	// Determine dial function based on proxy-network
	var dialFn awsapi.DialContextFunc
	if m.cfg.ProxyNetwork == "vm" && m.agentMux != nil {
		dialFn = awsapi.NewVsockDialer(m.agentMux)
		logger.Info("Proxy network: vm (via VM NAT)")
	} else {
		logger.Info("Proxy network: direct")
	}

	// Create unified AWS client
	m.awsClient = awsapi.NewClient(*awsCfg, dialFn)
	m.ec2Client = m.awsClient.EC2Client()

	// Resolve instance ID
	instanceID := m.cfg.InstanceID
	instanceState := ""
	if instanceID == "" && m.cfg.Name != "" {
		resolvedID, state, err := m.awsClient.ResolveInstanceByName(ctx, m.cfg.Name)
		if err != nil {
			return fmt.Errorf("failed to resolve instance: %w", err)
		}
		instanceID = resolvedID
		instanceState = state
		logger.Info("Resolved instance", "name", m.cfg.Name, "id", resolvedID, "state", state)
	} else if instanceID != "" {
		if m.cfg.AutoStart {
			state, err := m.awsClient.GetInstanceState(ctx, instanceID)
			if err != nil {
				return fmt.Errorf("failed to get instance state: %w", err)
			}
			instanceState = state
		}
	}
	m.resolvedInstanceID = instanceID

	// Auto-start instance if stopped or stopping
	if instanceID != "" && m.cfg.AutoStart && (instanceState == "stopped" || instanceState == "stopping") {
		// If stopping, wait for it to fully stop first
		if instanceState == "stopping" {
			logger.Info("Instance is stopping, waiting for stopped state...", "instance", instanceID)
			if err := m.awsClient.WaitForInstanceState(ctx, instanceID, "stopped", 3*time.Minute); err != nil {
				return fmt.Errorf("failed to wait for instance to stop: %w", err)
			}
		}
		logger.Info("Starting instance...", "instance", instanceID)
		if err := m.awsClient.StartInstanceAndWait(ctx, instanceID, 5*time.Minute); err != nil {
			return fmt.Errorf("failed to start instance: %w", err)
		}
		logger.Info("Instance is now running, waiting for SSM agent...", "instance", instanceID)
		if err := m.awsClient.WaitForSSMAgent(ctx, instanceID, 3*time.Minute); err != nil {
			return fmt.Errorf("failed to wait for SSM agent: %w", err)
		}
	}

	// Create and start SSM backend
	if instanceID != "" {
		backendCfg := &awsapi.SSMBackendConfig{
			InstanceID:           instanceID,
			Region:               m.cfg.Region,
			SSHUser:              m.cfg.SSHUser,
			SSHKeyPath:           m.cfg.SSHKeyPath,
			AutoStartEC2:         m.cfg.AutoStart,
			SSHKeepaliveInterval: m.cfg.SSHKeepaliveInterval,
		}

		ssmBe := m.awsClient.NewSSMBackend(backendCfg)

		if err := ssmBe.Start(m.ctx); err != nil {
			return fmt.Errorf("failed to start SSM backend: %w", err)
		}
		m.backend = ssmBe

		// Store credentials for backend
		creds := m.credProv.GetLastCredentials()
		ssmBe.SetCredentials(creds)

		// Start credential refresh loop
		go m.credentialRefreshLoop(m.ctx)
	}

	return nil
}

// EnsureInitialized performs lazy AWS initialization on first proxy request
func (m *Manager) EnsureInitialized(ctx context.Context) error {
	m.awsInitMu.Lock()
	defer m.awsInitMu.Unlock()

	if m.awsInitialized {
		return nil
	}

	// Enable auto-start when resuming from suspend
	wasSuspended := m.idleTracker != nil && m.idleTracker.IsSuspended()
	if wasSuspended {
		m.cfg.AutoStart = true
		logger.Info("Resuming from idle suspend: auto-start enabled")
	}

	logger.Info("Lazy initialization: starting AWS credential and instance resolution...")

	initFn := m.initializeProxy
	if m.initializeProxyFn != nil {
		initFn = m.initializeProxyFn
	}
	if err := initFn(ctx); err != nil {
		// Cleanup credential provider to avoid leaking watchers on retry
		if m.credProv != nil {
			m.credProv.Stop()
		}
		m.credProv = credentials.NewProvider(m.cfg.Profile, m.cfg.Region)

		// Re-create context for next attempt
		m.cancel()
		m.ctx, m.cancel = context.WithCancel(context.Background())

		m.initErr = err
		close(m.initDone)
		m.initDone = make(chan struct{})
		return err
	}

	// Update socks5 server's backend reference
	if m.socks5 != nil && m.backend != nil {
		m.socks5.SetBackend(m.backend)
	}

	m.awsInitialized = true
	m.initErr = nil
	close(m.initDone)

	// Clear suspended state. Idle timer will be restarted by the first
	// successful proxy Dial (via socks5.dial -> idleTracker.Touch).
	// Do NOT start the timer here because SSM backend connects lazily on
	// first Dial, and the connection may take longer than the idle timeout.
	if m.idleTracker != nil {
		m.idleTracker.ClearSuspended()
	}

	logger.Info("Lazy initialization completed")
	return nil
}

// InitDone returns a channel that is closed when initialization completes
func (m *Manager) InitDone() <-chan struct{} {
	return m.initDone
}

// InitError returns the initialization error, if any
func (m *Manager) InitError() error {
	return m.initErr
}

// GetBackend returns the current backend (may be nil)
func (m *Manager) GetBackend() backend.Backend {
	m.awsInitMu.Lock()
	defer m.awsInitMu.Unlock()
	return m.backend
}

// IsInitialized returns true if AWS initialization is complete
func (m *Manager) IsInitialized() bool {
	m.awsInitMu.Lock()
	defer m.awsInitMu.Unlock()
	return m.awsInitialized
}

// suspend stops the EC2 instance and resets initialization state
func (m *Manager) suspend() {
	m.awsInitMu.Lock()
	defer m.awsInitMu.Unlock()

	if !m.awsInitialized {
		return
	}

	logger.Info("Idle timeout: suspending EC2 instance...", "instance", m.resolvedInstanceID)

	// 1. Reset initialization state
	m.awsInitialized = false
	m.initDone = make(chan struct{})
	m.initErr = nil

	// 2. Close backend
	if m.backend != nil {
		m.backend.Close()
		m.backend = nil
	}

	// 3. Stop EC2 instance
	if m.resolvedInstanceID != "" && m.ec2Client != nil {
		instMgr := ec2pkg.NewInstanceManager(m.ec2Client)
		if err := instMgr.Stop(context.Background(), m.resolvedInstanceID); err != nil {
			logger.Warn("failed to stop instance during suspend", "error", err)
		} else {
			logger.Info("EC2 instance stop initiated", "instance", m.resolvedInstanceID)
		}
	}

	// 4. Stop and re-create credential provider
	if m.credProv != nil {
		m.credProv.Stop()
	}
	m.credProv = credentials.NewProvider(m.cfg.Profile, m.cfg.Region)

	// 5. Re-create context
	m.cancel()
	m.ctx, m.cancel = context.WithCancel(context.Background())

	logger.Info("Suspend complete, waiting for new proxy requests to trigger re-initialization")
}

// credentialRefreshLoop sends updated credentials to the backend
func (m *Manager) credentialRefreshLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case creds := <-m.credProv.RefreshChannel():
			m.awsInitMu.Lock()
			be := m.backend
			m.awsInitMu.Unlock()
			if be == nil {
				continue
			}
			logger.Debug("Sending updated credentials to backend...")
			if err := be.OnCredentialUpdate(creds); err != nil {
				logger.Error("Failed to update backend credentials", "error", err)
			} else {
				logger.Debug("Backend credentials updated successfully")
			}
		}
	}
}

// Stop stops the proxy
func (m *Manager) Stop() error {
	m.cancel()

	if m.idleTracker != nil {
		m.idleTracker.Stop()
	}

	if m.socks5 != nil {
		m.socks5.Stop()
	}

	if m.agentMux != nil {
		m.agentMux.SendShutdown()
		m.agentMux.Close()
	}

	if m.vm != nil {
		m.vm.Stop()
		m.vm.Cleanup()
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

// Ensure Manager implements LazyInitializer
var _ LazyInitializer = (*Manager)(nil)
