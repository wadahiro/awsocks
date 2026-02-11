package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindSSHKey_Ed25519First(t *testing.T) {
	tmpDir := t.TempDir()

	// Create both key types
	ed25519Path := filepath.Join(tmpDir, "id_ed25519")
	rsaPath := filepath.Join(tmpDir, "id_rsa")

	require.NoError(t, os.WriteFile(ed25519Path, []byte("ed25519 key"), 0600))
	require.NoError(t, os.WriteFile(rsaPath, []byte("rsa key"), 0600))

	finder := NewSSHKeyFinder(tmpDir)
	keyPath, err := finder.FindDefaultKey()

	require.NoError(t, err)
	assert.Equal(t, ed25519Path, keyPath)
}

func TestFindSSHKey_FallbackToRSA(t *testing.T) {
	tmpDir := t.TempDir()

	// Create only RSA key
	rsaPath := filepath.Join(tmpDir, "id_rsa")
	require.NoError(t, os.WriteFile(rsaPath, []byte("rsa key"), 0600))

	finder := NewSSHKeyFinder(tmpDir)
	keyPath, err := finder.FindDefaultKey()

	require.NoError(t, err)
	assert.Equal(t, rsaPath, keyPath)
}

func TestFindSSHKey_FallbackToECDSA(t *testing.T) {
	tmpDir := t.TempDir()

	// Create only ECDSA key
	ecdsaPath := filepath.Join(tmpDir, "id_ecdsa")
	require.NoError(t, os.WriteFile(ecdsaPath, []byte("ecdsa key"), 0600))

	finder := NewSSHKeyFinder(tmpDir)
	keyPath, err := finder.FindDefaultKey()

	require.NoError(t, err)
	assert.Equal(t, ecdsaPath, keyPath)
}

func TestFindSSHKey_NotFound_ReturnsError(t *testing.T) {
	tmpDir := t.TempDir()

	finder := NewSSHKeyFinder(tmpDir)
	_, err := finder.FindDefaultKey()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no SSH key found")
}

func TestFindSSHKey_SkipsPublicKeys(t *testing.T) {
	tmpDir := t.TempDir()

	// Create only public key
	pubPath := filepath.Join(tmpDir, "id_ed25519.pub")
	require.NoError(t, os.WriteFile(pubPath, []byte("public key"), 0600))

	finder := NewSSHKeyFinder(tmpDir)
	_, err := finder.FindDefaultKey()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no SSH key found")
}

func TestFindSSHKey_PriorityOrder(t *testing.T) {
	tests := []struct {
		name         string
		files        []string
		expectedFile string
	}{
		{
			name:         "ed25519 has highest priority",
			files:        []string{"id_ed25519", "id_rsa", "id_ecdsa"},
			expectedFile: "id_ed25519",
		},
		{
			name:         "rsa over ecdsa",
			files:        []string{"id_rsa", "id_ecdsa"},
			expectedFile: "id_rsa",
		},
		{
			name:         "ecdsa only",
			files:        []string{"id_ecdsa"},
			expectedFile: "id_ecdsa",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			for _, f := range tt.files {
				path := filepath.Join(tmpDir, f)
				require.NoError(t, os.WriteFile(path, []byte("key content"), 0600))
			}

			finder := NewSSHKeyFinder(tmpDir)
			keyPath, err := finder.FindDefaultKey()

			require.NoError(t, err)
			assert.Equal(t, filepath.Join(tmpDir, tt.expectedFile), keyPath)
		})
	}
}

func TestFindSSHKey_DefaultSSHDir(t *testing.T) {
	// Test that DefaultSSHKeyFinder uses ~/.ssh
	finder := DefaultSSHKeyFinder()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	expectedDir := filepath.Join(home, ".ssh")
	assert.Equal(t, expectedDir, finder.sshDir)
}

func TestFindSSHKey_ExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	finder := NewSSHKeyFinder("~/.ssh")
	assert.Equal(t, filepath.Join(home, ".ssh"), finder.sshDir)
}
