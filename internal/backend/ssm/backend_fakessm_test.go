package ssm_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/backend/ssm/datachannel"
	"github.com/wadahiro/awsocks/internal/testutil/fakessm"
)

// TestFakeSSM_DataChannelIntegration tests the DataChannel component used by MuxSSH
// against the fake SSM server. This validates the core SSM communication layer.
func TestFakeSSM_DataChannelIntegration(t *testing.T) {
	server := fakessm.NewServer(nil)
	url := server.Start()
	defer server.Close()

	dc := datachannel.NewDataChannel()
	dc.SetClientVersion("1.0.0")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	// Send OpenDataChannel (simulating what MuxSSH Backend does)
	err = dc.SendJSON(map[string]string{
		"MessageSchemaVersion": "1.0",
		"RequestId":            "test-session-id",
		"TokenValue":           "test-token",
	})
	require.NoError(t, err)

	// Wait for handshake
	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	select {
	case <-handshakeComplete:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("handshake timed out")
	}

	assert.True(t, dc.IsHandshakeComplete())

	// Set up bidirectional data transfer (simulating SSH data)
	receivedData := make(chan []byte, 100)
	dc.SetOnOutputData(func(data []byte) {
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		receivedData <- dataCopy
	})

	// Send data (simulating SSH packets)
	testData := []byte("SSH-2.0-OpenSSH_8.9\r\n")
	err = dc.SendInputData(testData)
	require.NoError(t, err)

	select {
	case data := <-receivedData:
		assert.Equal(t, testData, data)
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive echoed data")
	}
}

// TestFakeSSM_HighVolumeDataTransfer simulates the high-volume data transfer
// that occurs during SSH sessions with many parallel channels
func TestFakeSSM_HighVolumeDataTransfer(t *testing.T) {
	server := fakessm.NewServer(nil)
	url := server.Start()
	defer server.Close()

	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-session",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	<-handshakeComplete

	var receivedCount int64
	dc.SetOnOutputData(func(data []byte) {
		atomic.AddInt64(&receivedCount, 1)
	})

	// Simulate high-volume data transfer (like SSH with many parallel operations)
	const messageCount = 500
	const messageSize = 1024

	start := time.Now()

	for i := 0; i < messageCount; i++ {
		data := make([]byte, messageSize)
		for j := range data {
			data[j] = byte(i % 256)
		}

		err = dc.SendInputData(data)
		require.NoError(t, err)

		// Small delay to simulate realistic packet pacing
		time.Sleep(time.Millisecond)
	}

	// Wait for all messages to be echoed back
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&receivedCount) >= messageCount {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	elapsed := time.Since(start)
	received := atomic.LoadInt64(&receivedCount)

	t.Logf("Sent %d messages, received %d echoes in %v", messageCount, received, elapsed)
	t.Logf("Throughput: %.2f messages/sec", float64(received)/elapsed.Seconds())

	assert.Equal(t, int64(messageCount), received, "all messages should be echoed")
}

// TestFakeSSM_ConnectionStability tests that the connection remains stable
// under sustained load (reproducing the SSM disconnection issue)
func TestFakeSSM_ConnectionStability(t *testing.T) {
	var connectCount, disconnectCount int64

	opts := &fakessm.ServerOptions{
		AgentVersion: "3.1.0.0",
		OnConnect: func(sessionID string) {
			atomic.AddInt64(&connectCount, 1)
		},
		OnDisconnect: func(sessionID string) {
			atomic.AddInt64(&disconnectCount, 1)
		},
	}

	server := fakessm.NewServer(opts)
	url := server.Start()
	defer server.Close()

	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	// Track disconnection events
	disconnected := make(chan struct{}, 1)
	dc.SetOnDisconnect(func() {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	})

	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-session",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	<-handshakeComplete

	// Consume echoed data
	dc.SetOnOutputData(func(data []byte) {})

	// Send data continuously for 5 seconds
	testDuration := 5 * time.Second
	endTime := time.Now().Add(testDuration)
	var sendCount int64

	for time.Now().Before(endTime) {
		err = dc.SendInputData([]byte("stability test data"))
		if err != nil {
			t.Fatalf("send error after %d messages: %v", sendCount, err)
		}
		atomic.AddInt64(&sendCount, 1)
		time.Sleep(10 * time.Millisecond)
	}

	// Check that connection is still alive
	select {
	case <-disconnected:
		t.Fatal("connection was unexpectedly disconnected during test")
	default:
		// Good - no disconnection
	}

	assert.True(t, dc.IsOpen(), "connection should still be open")
	assert.Equal(t, int64(1), atomic.LoadInt64(&connectCount), "should have exactly 1 connection")
	assert.Equal(t, int64(0), atomic.LoadInt64(&disconnectCount), "should have no disconnections during test")

	t.Logf("Sent %d messages over %v without disconnection", sendCount, testDuration)
}

// TestFakeSSM_ReconnectionScenario tests what happens when reconnection is needed
func TestFakeSSM_ReconnectionScenario(t *testing.T) {
	server := fakessm.NewServer(nil)
	url := server.Start()

	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-session",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	<-handshakeComplete

	// Track disconnection
	disconnected := make(chan struct{})
	dc.SetOnDisconnect(func() {
		close(disconnected)
	})

	// Simulate server-side disconnection (like SSM timeout or error)
	server.Close()

	// Wait for client to detect disconnection
	select {
	case <-disconnected:
		// Expected - OnDisconnect callback was triggered
		// This is the key signal that MuxSSH Backend uses to trigger reconnection
	case <-time.After(5 * time.Second):
		t.Fatal("client did not detect server disconnection")
	}

	// Note: IsOpen() may still return true briefly because the WebSocket
	// connection object exists but is closed. The important thing is that
	// OnDisconnect was called, which triggers reconnection in MuxSSH Backend.
}

// TestFakeSSM_LargePayload tests handling of large data payloads
func TestFakeSSM_LargePayload(t *testing.T) {
	server := fakessm.NewServer(nil)
	url := server.Start()
	defer server.Close()

	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-session",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	<-handshakeComplete

	receivedData := make(chan []byte, 10)
	dc.SetOnOutputData(func(data []byte) {
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		receivedData <- dataCopy
	})

	// Test with large payload (simulating file transfer over SSH)
	largeData := make([]byte, 16*1024) // 16KB
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	err = dc.SendInputData(largeData)
	require.NoError(t, err)

	select {
	case data := <-receivedData:
		assert.Equal(t, largeData, data, "large payload should be echoed correctly")
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive echoed large payload")
	}
}
