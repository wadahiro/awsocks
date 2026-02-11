//go:build linux

// VM Agent - Runs as Linux init (PID 1) inside the VM
// Handles vsock communication with host and SSM connections to EC2
package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/vishvananda/netlink"
	"github.com/wadahiro/awsocks/internal/agent"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/protocol"
)

var logger = log.For(log.ComponentAgent)

const (
	vsockPort    = 5000
	networkIF    = "eth0"
	staticIP     = "192.168.64.10/24"
	gatewayIP    = "192.168.64.1"
	nameserverIP = "8.8.8.8"
)

func main() {
	logger.Info("Starting VM agent as init (PID 1)...")

	// 1. Mount essential filesystems
	if err := mountFilesystems(); err != nil {
		logger.Error("Failed to mount filesystems", "error", err)
	}

	// 2. Configure network with static IP
	if err := setupNetwork(); err != nil {
		logger.Error("Failed to setup network", "error", err)
	}

	// 3. Start zombie process reaper
	go reapZombies()

	// 4. Connect to host via vsock
	logger.Info("Connecting to host via vsock", "port", vsockPort)
	conn, err := connectToHost()
	if err != nil {
		logger.Error("Failed to connect to host", "error", err)
		// Keep running to prevent kernel panic
		select {}
	}
	defer conn.Close()

	logger.Info("Connected to host via vsock")

	// 5. Create and run agent (without backend for now - will be configured via protocol)
	agentInstance := agent.NewWithoutBackend(conn)
	if err := agentInstance.Run(); err != nil {
		logger.Error("Agent error", "error", err)
	}

	// Never exit - would cause kernel panic
	select {}
}

func mountFilesystems() error {
	mounts := []struct {
		source string
		target string
		fstype string
		flags  uintptr
	}{
		{"proc", "/proc", "proc", 0},
		{"sysfs", "/sys", "sysfs", 0},
		{"devtmpfs", "/dev", "devtmpfs", 0},
	}

	for _, m := range mounts {
		// Create mount point if it doesn't exist
		if err := os.MkdirAll(m.target, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", m.target, err)
		}

		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, ""); err != nil {
			// Ignore already mounted errors
			if err != syscall.EBUSY {
				return fmt.Errorf("failed to mount %s: %w", m.target, err)
			}
		}
		logger.Debug("Mounted", "target", m.target)
	}

	return nil
}

func setupNetwork() error {
	// Configure loopback interface first
	lo, err := netlink.LinkByName("lo")
	if err == nil {
		loAddr, _ := netlink.ParseAddr("127.0.0.1/8")
		netlink.AddrAdd(lo, loAddr)
		netlink.LinkSetUp(lo)
		logger.Debug("Configured loopback interface")
	}

	// Find network interface
	link, err := netlink.LinkByName(networkIF)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %w", networkIF, err)
	}

	// Check if already configured (by init script DHCP)
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err == nil && len(addrs) > 0 {
		logger.Debug("Network already configured", "ip", addrs[0].IP.String())
		return nil
	}

	// Fallback to static IP if DHCP didn't work
	logger.Debug("No IP configured, using static IP fallback...")

	// Parse IP address
	addr, err := netlink.ParseAddr(staticIP)
	if err != nil {
		return fmt.Errorf("failed to parse IP %s: %w", staticIP, err)
	}

	// Add IP address to interface
	if err := netlink.AddrAdd(link, addr); err != nil {
		// Ignore if address already exists
		if err.Error() != "file exists" {
			return fmt.Errorf("failed to add address: %w", err)
		}
	}

	// Bring interface up
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up interface: %w", err)
	}

	logger.Debug("Configured network interface", "interface", networkIF, "ip", staticIP)

	// Add default route
	gw := net.ParseIP(gatewayIP)
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        gw,
	}
	if err := netlink.RouteAdd(route); err != nil {
		// Ignore if route already exists
		if err.Error() != "file exists" {
			return fmt.Errorf("failed to add default route: %w", err)
		}
	}

	logger.Debug("Added default route", "gateway", gatewayIP)

	// Configure DNS
	if err := os.WriteFile("/etc/resolv.conf", []byte(fmt.Sprintf("nameserver %s\n", nameserverIP)), 0644); err != nil {
		logger.Warn("failed to write resolv.conf", "error", err)
	}

	return nil
}

func reapZombies() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		logger.Debug("Reaped zombie process", "pid", pid)
	}
}

func connectToHost() (net.Conn, error) {
	// Retry connection with backoff
	var conn net.Conn
	var err error

	for i := 0; i < 30; i++ {
		conn, err = vsock.Dial(vsock.Host, vsockPort, nil)
		if err == nil {
			return conn, nil
		}
		logger.Debug("Waiting for host vsock", "attempt", i+1, "maxAttempts", 30)
		time.Sleep(time.Second)
	}

	return nil, fmt.Errorf("failed to connect after 30 attempts: %w", err)
}

// Ensure protocol package is imported for message types
var _ = protocol.MsgConnect
