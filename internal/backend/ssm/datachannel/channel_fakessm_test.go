package datachannel_test

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

// TestDataChannel_FakeSSM_FullHandshake tests the complete handshake flow
// with a realistic fake SSM server
func TestDataChannel_FakeSSM_FullHandshake(t *testing.T) {
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

	// Send OpenDataChannel
	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-session-id",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	// Wait for handshake to complete
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
}

// TestDataChannel_FakeSSM_BidirectionalData tests data exchange in both directions
func TestDataChannel_FakeSSM_BidirectionalData(t *testing.T) {
	server := fakessm.NewServer(nil)
	url := server.Start()
	defer server.Close()

	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-request",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	<-handshakeComplete

	// Set up data receiver
	receivedData := make(chan []byte, 10)
	dc.SetOnOutputData(func(data []byte) {
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		receivedData <- dataCopy
	})

	// Send multiple messages and verify echo
	testMessages := [][]byte{
		[]byte("Hello, SSM!"),
		[]byte("Second message"),
		[]byte("Third message with more data"),
	}

	for _, msg := range testMessages {
		err = dc.SendInputData(msg)
		require.NoError(t, err)

		select {
		case data := <-receivedData:
			assert.Equal(t, msg, data)
		case <-time.After(2 * time.Second):
			t.Fatalf("did not receive echo for message: %s", msg)
		}
	}
}

// TestDataChannel_FakeSSM_HighThroughput tests sending many messages rapidly
func TestDataChannel_FakeSSM_HighThroughput(t *testing.T) {
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
		RequestID:            "test-request",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	<-handshakeComplete

	// Track received messages
	var receivedCount int64
	dc.SetOnOutputData(func(data []byte) {
		atomic.AddInt64(&receivedCount, 1)
	})

	// Send 100 messages rapidly
	const messageCount = 100
	for i := 0; i < messageCount; i++ {
		err = dc.SendInputData([]byte("test message"))
		require.NoError(t, err)
	}

	// Wait for all echoes
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&receivedCount) >= messageCount {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	assert.Equal(t, int64(messageCount), atomic.LoadInt64(&receivedCount))
}

// TestDataChannel_FakeSSM_ACKHandling tests that ACKs are properly exchanged
func TestDataChannel_FakeSSM_ACKHandling(t *testing.T) {
	var ackCount int64
	opts := &fakessm.ServerOptions{
		AgentVersion: "3.1.0.0",
		OnMessage: func(sessionID string, msg []byte) {
			// Count ACK messages from client
			clientMsg := &datachannel.ClientMessage{}
			if err := clientMsg.Deserialize(msg); err == nil {
				if clientMsg.MessageType == datachannel.AcknowledgeMessage {
					atomic.AddInt64(&ackCount, 1)
				}
			}
		},
	}

	server := fakessm.NewServer(opts)
	url := server.Start()
	defer server.Close()

	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-request",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	<-handshakeComplete

	// Consume echoed data to avoid blocking
	dc.SetOnOutputData(func(data []byte) {})

	// Send data to trigger ACK from server
	for i := 0; i < 5; i++ {
		err = dc.SendInputData([]byte("test"))
		require.NoError(t, err)
	}

	time.Sleep(500 * time.Millisecond)

	// Client should have sent ACKs for:
	// - HandshakeRequest
	// - HandshakeComplete
	// - 5 echoed data messages
	assert.GreaterOrEqual(t, atomic.LoadInt64(&ackCount), int64(2), "expected at least 2 ACKs (handshake)")
}

// TestDataChannel_FakeSSM_PendingMessageTracking tests that pending messages are tracked and cleared on ACK
func TestDataChannel_FakeSSM_PendingMessageTracking(t *testing.T) {
	server := fakessm.NewServer(nil)
	url := server.Start()
	defer server.Close()

	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-request",
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

	// Send multiple messages
	for i := 0; i < 10; i++ {
		err = dc.SendInputData([]byte("test"))
		require.NoError(t, err)
	}

	// Wait for ACKs to be processed
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if dc.PendingCount() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// All messages should have been ACKed
	assert.Equal(t, 0, dc.PendingCount(), "all pending messages should be cleared after ACK")
}

// TestDataChannel_FakeSSM_ParallelConnections tests multiple parallel DataChannel connections
func TestDataChannel_FakeSSM_ParallelConnections(t *testing.T) {
	server := fakessm.NewServer(nil)
	url := server.Start()
	defer server.Close()

	const numConnections = 5
	errors := make(chan error, numConnections)
	done := make(chan struct{}, numConnections)

	for i := 0; i < numConnections; i++ {
		go func() {
			dc := datachannel.NewDataChannel()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := dc.Open(ctx, url); err != nil {
				errors <- err
				return
			}
			defer dc.Close()

			err := dc.SendJSON(fakessm.OpenDataChannelInput{
				MessageSchemaVersion: "1.0",
				RequestID:            "test-request",
				TokenValue:           "test-token",
			})
			if err != nil {
				errors <- err
				return
			}

			handshakeComplete := make(chan struct{})
			dc.SetOnHandshakeComplete(func() {
				close(handshakeComplete)
			})

			select {
			case <-handshakeComplete:
			case <-ctx.Done():
				errors <- ctx.Err()
				return
			}

			// Send some data
			for j := 0; j < 10; j++ {
				if err := dc.SendInputData([]byte("test")); err != nil {
					errors <- err
					return
				}
			}

			done <- struct{}{}
		}()
	}

	// Wait for all connections to complete
	successCount := 0
	for i := 0; i < numConnections; i++ {
		select {
		case err := <-errors:
			t.Errorf("connection error: %v", err)
		case <-done:
			successCount++
		case <-time.After(15 * time.Second):
			t.Fatal("timeout waiting for connections")
		}
	}

	assert.Equal(t, numConnections, successCount, "all connections should complete successfully")
}
