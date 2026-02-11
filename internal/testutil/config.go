// Package testutil provides utilities for integration testing
package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/joho/godotenv"
)

// TestConfig holds integration test configuration
type TestConfig struct {
	InstanceName string
	Region       string
	Profile      string
	SSHUser      string
	SSHKeyPath   string
}

// LoadTestConfig loads test configuration from environment variables.
// Falls back to .env.test file if present.
func LoadTestConfig(t *testing.T) *TestConfig {
	t.Helper()

	// Try to load .env.test file from various locations
	// (tests may run from different directories)
	envFiles := []string{
		".env.test",
		"../../.env.test",
		"../../../.env.test",
		"../../../../.env.test",
	}
	for _, f := range envFiles {
		if err := godotenv.Load(f); err == nil {
			break
		}
	}

	cfg := &TestConfig{
		InstanceName: getEnvOrSkip(t, "TEST_INSTANCE_NAME"),
		Region:       getEnvOrDefault("TEST_REGION", "ap-northeast-1"),
		Profile:      getEnvOrSkip(t, "TEST_PROFILE"),
		SSHUser:      getEnvOrDefault("TEST_SSH_USER", "ec2-user"),
		SSHKeyPath:   expandPath(getEnvOrSkip(t, "TEST_SSH_KEY_PATH")),
	}

	return cfg
}

func getEnvOrSkip(t *testing.T, key string) string {
	t.Helper()
	val := os.Getenv(key)
	if val == "" {
		t.Skipf("skipping: %s not set", key)
	}
	return val
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func expandPath(path string) string {
	if len(path) > 0 && path[0] == '~' {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}
