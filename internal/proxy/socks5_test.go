package proxy

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSOCKS5Server_StopClosesListener(t *testing.T) {
	// Create a mock agent connection (we won't actually use it)
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	server := NewSOCKS5Server("127.0.0.1:0", agentClient, nil)

	// Start server in goroutine
	startErr := make(chan error, 1)
	go func() {
		startErr <- server.Start()
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	// Verify listener is set
	server.listenerMu.Lock()
	listener := server.listener
	server.listenerMu.Unlock()
	require.NotNil(t, listener, "listener should be set after Start()")

	// Get the actual listen address
	addr := listener.Addr().String()

	// Verify we can connect
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err, "should be able to connect to server")
	conn.Close()

	// Stop the server
	server.Stop()

	// Wait for Start() to return
	select {
	case err := <-startErr:
		// Start() should return with an error (listener closed)
		assert.Error(t, err, "Start() should return error when listener is closed")
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after Stop() was called")
	}

	// Verify we can no longer connect
	_, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
	assert.Error(t, err, "should not be able to connect after Stop()")
}

func TestSOCKS5Server_StopBeforeStart(t *testing.T) {
	agentServer, agentClient := net.Pipe()
	defer agentServer.Close()
	defer agentClient.Close()

	server := NewSOCKS5Server("127.0.0.1:0", agentClient, nil)

	// Stop before Start should not panic
	assert.NotPanics(t, func() {
		server.Stop()
	})
}
