package ssm

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// NewSSHClientConfig creates an SSH client config from the backend config
func NewSSHClientConfig(cfg *Config) (*ssh.ClientConfig, error) {
	var authMethods []ssh.AuthMethod

	// Try key-based authentication
	if cfg.SSHKeyPath != "" {
		keyAuth, err := loadPrivateKey(cfg.SSHKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load SSH key: %w", err)
		}
		authMethods = append(authMethods, keyAuth)
	}

	// Try password authentication
	if cfg.SSHPassword != "" {
		authMethods = append(authMethods, ssh.Password(cfg.SSHPassword))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no SSH authentication method configured")
	}

	return &ssh.ClientConfig{
		User:            cfg.SSHUser,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}, nil
}

// loadPrivateKey loads an SSH private key from file
func loadPrivateKey(path string) (ssh.AuthMethod, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	return ssh.PublicKeys(signer), nil
}
