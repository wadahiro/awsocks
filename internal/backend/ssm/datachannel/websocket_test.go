package datachannel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func TestWebSocketChannel_Open(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		// Echo loop
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			err = conn.WriteMessage(msgType, data)
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	// Convert http:// to ws://
	wsURL := "ws" + server.URL[4:]

	channel := NewWebSocketChannel()
	ctx := context.Background()

	err := channel.Open(ctx, wsURL)
	require.NoError(t, err)
	assert.True(t, channel.IsOpen())

	err = channel.Close()
	require.NoError(t, err)
	assert.False(t, channel.IsOpen())
}

func TestWebSocketChannel_SendMessage(t *testing.T) {
	var received []byte
	var receivedMu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		receivedMu.Lock()
		received = data
		receivedMu.Unlock()
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	channel := NewWebSocketChannel()
	ctx := context.Background()

	err := channel.Open(ctx, wsURL)
	require.NoError(t, err)
	defer channel.Close()

	testData := []byte("test message")
	err = channel.SendMessage(testData)
	require.NoError(t, err)

	// Wait for message to be received
	time.Sleep(100 * time.Millisecond)

	receivedMu.Lock()
	defer receivedMu.Unlock()
	assert.Equal(t, testData, received)
}

func TestWebSocketChannel_ReceiveLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		// Send test messages
		conn.WriteMessage(websocket.BinaryMessage, []byte("message 1"))
		conn.WriteMessage(websocket.BinaryMessage, []byte("message 2"))
		conn.WriteMessage(websocket.BinaryMessage, []byte("message 3"))

		// Keep connection open for a bit
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[4:]

	channel := NewWebSocketChannel()
	ctx := context.Background()

	var messages [][]byte
	var messagesMu sync.Mutex

	channel.SetOnMessage(func(data []byte) {
		messagesMu.Lock()
		// Make a copy to avoid data race
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		messages = append(messages, dataCopy)
		messagesMu.Unlock()
	})

	err := channel.Open(ctx, wsURL)
	require.NoError(t, err)

	// Start receive loop
	go channel.StartReceiving()

	// Wait for messages
	time.Sleep(300 * time.Millisecond)

	channel.Close()

	messagesMu.Lock()
	defer messagesMu.Unlock()

	require.Len(t, messages, 3)
	assert.Equal(t, []byte("message 1"), messages[0])
	assert.Equal(t, []byte("message 2"), messages[1])
	assert.Equal(t, []byte("message 3"), messages[2])
}

func TestWebSocketChannel_SendMessage_Closed(t *testing.T) {
	channel := NewWebSocketChannel()

	// Try to send without opening
	err := channel.SendMessage([]byte("test"))
	assert.Error(t, err)
}

func TestWebSocketChannel_Open_InvalidURL(t *testing.T) {
	channel := NewWebSocketChannel()
	ctx := context.Background()

	err := channel.Open(ctx, "ws://invalid-host-that-does-not-exist:9999")
	assert.Error(t, err)
}

func TestWebSocketChannel_Open_Timeout(t *testing.T) {
	channel := NewWebSocketChannel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Try to connect to a non-responsive endpoint
	err := channel.Open(ctx, "ws://10.255.255.1:9999")
	assert.Error(t, err)
}

func TestWebSocketChannel_Close_Already_Closed(t *testing.T) {
	channel := NewWebSocketChannel()

	// Close without opening - should not panic
	err := channel.Close()
	assert.NoError(t, err)
}
