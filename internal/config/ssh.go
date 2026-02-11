package config

import (
	"errors"
	"os"
	"path/filepath"
)

// SSH key search priority order
var sshKeyPriority = []string{
	"id_ed25519",
	"id_rsa",
	"id_ecdsa",
}

// ErrNoSSHKey is returned when no SSH key is found in the search path.
var ErrNoSSHKey = errors.New("no SSH key found")

// SSHKeyFinder searches for SSH keys in a given directory.
type SSHKeyFinder struct {
	sshDir string
}

// NewSSHKeyFinder creates a new SSHKeyFinder for the specified directory.
// The path can include ~ which will be expanded to the user's home directory.
func NewSSHKeyFinder(sshDir string) *SSHKeyFinder {
	return &SSHKeyFinder{
		sshDir: expandTilde(sshDir),
	}
}

// DefaultSSHKeyFinder creates a SSHKeyFinder for the default ~/.ssh directory.
func DefaultSSHKeyFinder() *SSHKeyFinder {
	home, err := os.UserHomeDir()
	if err != nil {
		return &SSHKeyFinder{sshDir: ".ssh"}
	}
	return &SSHKeyFinder{
		sshDir: filepath.Join(home, ".ssh"),
	}
}

// FindDefaultKey searches for an SSH private key in the configured directory.
// It searches in priority order: ed25519 > rsa > ecdsa
// Returns the path to the first key found, or ErrNoSSHKey if no key is found.
func (f *SSHKeyFinder) FindDefaultKey() (string, error) {
	for _, keyName := range sshKeyPriority {
		keyPath := filepath.Join(f.sshDir, keyName)
		if f.isValidPrivateKey(keyPath) {
			return keyPath, nil
		}
	}
	return "", ErrNoSSHKey
}

// isValidPrivateKey checks if a file exists and is a valid private key.
// It excludes public key files (*.pub).
func (f *SSHKeyFinder) isValidPrivateKey(path string) bool {
	// Skip public keys
	if filepath.Ext(path) == ".pub" {
		return false
	}

	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	// Must be a regular file
	if !info.Mode().IsRegular() {
		return false
	}

	return true
}
