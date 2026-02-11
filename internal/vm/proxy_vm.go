//go:build darwin

// Package vm provides VM management using macOS Virtualization.framework
package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/Code-Hex/vz/v3"
	"github.com/wadahiro/awsocks/internal/vm/assets"
)

const (
	vsockPort = 5000
)

// ProxyVM represents a VM configured for proxy operations with vsock support
type ProxyVM struct {
	vm           *vz.VirtualMachine
	config       *vz.VirtualMachineConfiguration
	socketDevice *vz.VirtioSocketDevice
	tempDir      string
}

// NewProxyVM creates a new proxy VM with vsock support
func NewProxyVM() (*ProxyVM, error) {
	// Create temporary directory for VM assets
	tempDir, err := os.MkdirTemp("", "awsocks-vm-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}

	// Extract embedded assets to temporary files
	kernelPath := filepath.Join(tempDir, "vmlinuz")
	initrdPath := filepath.Join(tempDir, "initramfs")

	if err := os.WriteFile(kernelPath, assets.Kernel, 0644); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to write kernel: %w", err)
	}

	if err := os.WriteFile(initrdPath, assets.Initramfs, 0644); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to write initramfs: %w", err)
	}

	// Configure boot loader - use rdinit for Go binary as init
	bootLoader, err := vz.NewLinuxBootLoader(
		kernelPath,
		vz.WithCommandLine("console=hvc0 rdinit=/init quiet loglevel=3"),
		vz.WithInitrd(initrdPath),
	)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to create boot loader: %w", err)
	}

	// Configure VM (1 CPU, 256MB RAM - smaller since no rootfs)
	config, err := vz.NewVirtualMachineConfiguration(bootLoader, 1, 256*1024*1024)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to create VM config: %w", err)
	}

	// Configure NAT network (required for SSM internet access)
	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to create NAT attachment: %w", err)
	}

	networkConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to create network config: %w", err)
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{
		networkConfig,
	})

	// Configure vsock device for host-VM communication
	socketConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to create socket config: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{
		socketConfig,
	})

	// Validate configuration
	validated, err := config.Validate()
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to validate config: %w", err)
	}
	if !validated {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("VM config validation failed")
	}

	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	return &ProxyVM{vm: vm, config: config, tempDir: tempDir}, nil
}

// Start starts the VM
func (p *ProxyVM) Start(ctx context.Context) error {
	if err := p.vm.Start(); err != nil {
		return fmt.Errorf("failed to start VM: %w", err)
	}

	// Get socket device after VM starts
	socketDevices := p.vm.SocketDevices()
	if len(socketDevices) == 0 {
		return fmt.Errorf("no socket devices available")
	}
	p.socketDevice = socketDevices[0]

	return nil
}

// WaitForAgent waits for the VM agent to connect via vsock
func (p *ProxyVM) WaitForAgent(ctx context.Context) (net.Conn, error) {
	if p.socketDevice == nil {
		return nil, fmt.Errorf("VM not started")
	}

	listener, err := p.socketDevice.Listen(vsockPort)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on vsock port %d: %w", vsockPort, err)
	}

	// Accept connection with context
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		conn, err := listener.Accept()
		ch <- result{conn, err}
	}()

	select {
	case <-ctx.Done():
		listener.Close()
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("failed to accept vsock connection: %w", r.err)
		}
		return r.conn, nil
	}
}

// Wait waits for the VM to stop
func (p *ProxyVM) Wait(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case state := <-p.vm.StateChangedNotify():
			switch state {
			case vz.VirtualMachineStateStopped:
				return nil
			case vz.VirtualMachineStateError:
				return fmt.Errorf("VM error")
			}
		}
	}
}

// Stop requests the VM to stop
func (p *ProxyVM) Stop() error {
	if p.vm.CanRequestStop() {
		ok, err := p.vm.RequestStop()
		if err != nil {
			return fmt.Errorf("failed to request stop: %w", err)
		}
		if !ok {
			return fmt.Errorf("stop request was not successful")
		}
	}
	return nil
}

// Cleanup removes temporary files
func (p *ProxyVM) Cleanup() {
	os.RemoveAll(p.tempDir)
}

// SocketDevice returns the vsock device for communication
func (p *ProxyVM) SocketDevice() *vz.VirtioSocketDevice {
	return p.socketDevice
}
