package datachannel

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/wadahiro/awsocks/internal/log"
)

var wsLogger = log.For(log.ComponentWebSocket)

// WebSocketChannel wraps a gorilla/websocket connection
type WebSocketChannel struct {
	conn      *websocket.Conn
	mu        sync.RWMutex
	writeMu   sync.Mutex // Protects concurrent writes
	onMessage func([]byte)
	closed    bool
}

// NewWebSocketChannel creates a new WebSocket channel
func NewWebSocketChannel() *WebSocketChannel {
	return &WebSocketChannel{}
}

// Open establishes a WebSocket connection
func (w *WebSocketChannel) Open(ctx context.Context, url string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 0, // Use context timeout
	}

	conn, resp, err := dialer.DialContext(ctx, url, http.Header{})
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	w.mu.Lock()
	w.conn = conn
	w.closed = false
	w.mu.Unlock()

	return nil
}

// OpenWithHeaders establishes a WebSocket connection with custom headers
func (w *WebSocketChannel) OpenWithHeaders(ctx context.Context, url string, headers http.Header) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 0, // Use context timeout
	}

	conn, resp, err := dialer.DialContext(ctx, url, headers)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	w.mu.Lock()
	w.conn = conn
	w.closed = false
	w.mu.Unlock()

	return nil
}

// Close closes the WebSocket connection
func (w *WebSocketChannel) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn == nil {
		return nil
	}

	w.closed = true
	err := w.conn.Close()
	w.conn = nil
	return err
}

// IsOpen returns whether the connection is open
func (w *WebSocketChannel) IsOpen() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.conn != nil && !w.closed
}

// SendMessage sends a binary message over the WebSocket
func (w *WebSocketChannel) SendMessage(data []byte) error {
	w.mu.RLock()
	conn := w.conn
	closed := w.closed
	w.mu.RUnlock()

	if conn == nil || closed {
		return fmt.Errorf("WebSocket not connected")
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, data)
}

// SendJSON sends a JSON message over the WebSocket
func (w *WebSocketChannel) SendJSON(v any) error {
	w.mu.RLock()
	conn := w.conn
	closed := w.closed
	w.mu.RUnlock()

	if conn == nil || closed {
		return fmt.Errorf("WebSocket not connected")
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return conn.WriteJSON(v)
}

// SetOnMessage sets the callback for incoming messages
func (w *WebSocketChannel) SetOnMessage(handler func([]byte)) {
	w.mu.Lock()
	w.onMessage = handler
	w.mu.Unlock()
}

// StartReceiving starts the receive loop (blocking)
func (w *WebSocketChannel) StartReceiving() {
	for {
		w.mu.RLock()
		conn := w.conn
		closed := w.closed
		onMessage := w.onMessage
		w.mu.RUnlock()

		if conn == nil || closed {
			return
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			// Connection closed or error
			wsLogger.Debug("ReadMessage error", "error", err)
			return
		}

		if onMessage != nil {
			onMessage(data)
		}
	}
}

// GetConnection returns the underlying connection (for testing)
func (w *WebSocketChannel) GetConnection() *websocket.Conn {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.conn
}
