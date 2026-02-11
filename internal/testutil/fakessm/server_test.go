package fakessm_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/backend/ssm/datachannel"
	"github.com/wadahiro/awsocks/internal/testutil/fakessm"
)

func TestFakeSSM_BasicConnection(t *testing.T) {
	// Create and start server
	server := fakessm.NewServer(nil)
	url := server.Start()
	defer server.Close()

	// Connect a client
	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	// Send OpenDataChannel
	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-request",
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
	assert.Equal(t, 1, server.SessionCount())
}

func TestFakeSSM_DataExchange(t *testing.T) {
	server := fakessm.NewServer(nil)
	url := server.Start()
	defer server.Close()

	dc := datachannel.NewDataChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dc.Open(ctx, url)
	require.NoError(t, err)
	defer dc.Close()

	// Send OpenDataChannel
	err = dc.SendJSON(fakessm.OpenDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestID:            "test-request",
		TokenValue:           "test-token",
	})
	require.NoError(t, err)

	// Wait for handshake
	handshakeComplete := make(chan struct{})
	dc.SetOnHandshakeComplete(func() {
		close(handshakeComplete)
	})

	select {
	case <-handshakeComplete:
	case <-time.After(5 * time.Second):
		t.Fatal("handshake timed out")
	}

	// Set up data receiver
	receivedData := make(chan []byte, 10)
	dc.SetOnOutputData(func(data []byte) {
		receivedData <- data
	})

	// Send data
	testData := []byte("Hello, SSM!")
	err = dc.SendInputData(testData)
	require.NoError(t, err)

	// Wait for echo
	select {
	case data := <-receivedData:
		assert.Equal(t, testData, data)
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive echoed data")
	}
}

func TestFakeSSM_MultipleMessages(t *testing.T) {
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
	var received [][]byte
	var mu sync.Mutex
	dc.SetOnOutputData(func(data []byte) {
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
	})

	// Send multiple messages
	messages := [][]byte{
		[]byte("Message 1"),
		[]byte("Message 2"),
		[]byte("Message 3"),
	}

	for _, msg := range messages {
		err = dc.SendInputData(msg)
		require.NoError(t, err)
	}

	// Wait for all echoes
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count >= len(messages) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	assert.Len(t, received, len(messages))
	mu.Unlock()
}

func TestFakeSSM_ParallelChannels(t *testing.T) {
	// Track connections and disconnections
	var connectedCount int
	var mu sync.Mutex

	opts := &fakessm.ServerOptions{
		AgentVersion: "3.1.0.0",
		OnConnect: func(sessionID string) {
			mu.Lock()
			connectedCount++
			mu.Unlock()
		},
	}

	server := fakessm.NewServer(opts)
	url := server.Start()
	defer server.Close()

	const numChannels = 10
	var wg sync.WaitGroup
	errors := make(chan error, numChannels)

	for i := 0; i < numChannels; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

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
			for j := 0; j < 5; j++ {
				err = dc.SendInputData([]byte("test data"))
				if err != nil {
					errors <- err
					return
				}
			}

			// Keep connection alive briefly
			time.Sleep(100 * time.Millisecond)
		}()
	}

	wg.Wait()
	close(errors)

	// Check for errors
	for err := range errors {
		t.Errorf("parallel channel error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, numChannels, connectedCount, "all channels should have connected")
}

func TestFakeSSM_ServerCallbacks(t *testing.T) {
	var connected, disconnected bool
	var receivedMsgCount int
	var mu sync.Mutex

	opts := &fakessm.ServerOptions{
		AgentVersion: "3.1.0.0",
		OnConnect: func(sessionID string) {
			mu.Lock()
			connected = true
			mu.Unlock()
		},
		OnMessage: func(sessionID string, msg []byte) {
			mu.Lock()
			receivedMsgCount++
			mu.Unlock()
		},
		OnDisconnect: func(sessionID string) {
			mu.Lock()
			disconnected = true
			mu.Unlock()
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

	// Send data
	err = dc.SendInputData([]byte("test"))
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// Close connection
	dc.Close()
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	assert.True(t, connected, "OnConnect should have been called")
	assert.True(t, disconnected, "OnDisconnect should have been called")
	assert.Greater(t, receivedMsgCount, 0, "should have received messages")
}

func TestFakeSSM_SessionEchoMode(t *testing.T) {
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

	// Disable echo mode for all sessions
	for _, session := range server.Sessions() {
		session.EchoMode = false
	}

	receivedData := make(chan []byte, 1)
	dc.SetOnOutputData(func(data []byte) {
		receivedData <- data
	})

	// Send data
	err = dc.SendInputData([]byte("should not echo"))
	require.NoError(t, err)

	// Should not receive echo
	select {
	case <-receivedData:
		t.Fatal("should not have received echo with EchoMode disabled")
	case <-time.After(200 * time.Millisecond):
		// Expected - no echo
	}
}
