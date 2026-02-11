//go:build !darwin

// Package vm provides VM management (stub for non-macOS platforms)
package vm

import (
	"context"
	"fmt"
	"net"
)

// ProxyVM is not supported on non-macOS platforms
type ProxyVM struct{}

// NewProxyVM returns an error on non-macOS platforms
func NewProxyVM() (*ProxyVM, error) {
	return nil, fmt.Errorf("VM mode is only supported on macOS")
}

// Start is not supported on non-macOS platforms
func (p *ProxyVM) Start(ctx context.Context) error {
	return fmt.Errorf("VM mode is only supported on macOS")
}

// WaitForAgent is not supported on non-macOS platforms
func (p *ProxyVM) WaitForAgent(ctx context.Context) (net.Conn, error) {
	return nil, fmt.Errorf("VM mode is only supported on macOS")
}

// Wait is not supported on non-macOS platforms
func (p *ProxyVM) Wait(ctx context.Context) error {
	return fmt.Errorf("VM mode is only supported on macOS")
}

// Stop is not supported on non-macOS platforms
func (p *ProxyVM) Stop() error {
	return fmt.Errorf("VM mode is only supported on macOS")
}

// Cleanup does nothing on non-macOS platforms
func (p *ProxyVM) Cleanup() {}
