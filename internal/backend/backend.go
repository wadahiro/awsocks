package backend

import (
	"context"
	"net"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Backend provides connections through various AWS services
type Backend interface {
	// Name returns the backend name for logging
	Name() string

	// Start initializes the backend
	Start(ctx context.Context) error

	// Dial establishes a connection to the target address
	Dial(ctx context.Context, network, address string) (net.Conn, error)

	// OnCredentialUpdate handles credential refresh
	OnCredentialUpdate(creds aws.Credentials) error

	// Close releases resources
	Close() error
}

// Config holds backend configuration
type Config struct {
	Type       string // "ssm", "eic", "direct"
	InstanceID string
	RemoteHost string
	RemotePort int
	Region     string
}
