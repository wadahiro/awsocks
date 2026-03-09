package datachannel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/wadahiro/awsocks/internal/log"
)

var wsLogger = log.For(log.ComponentWebSocket)

// DialContextFunc is a function that dials a network connection
type DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// WebSocketChannel wraps a gorilla/websocket connection
type WebSocketChannel struct {
	conn          *websocket.Conn
	mu            sync.RWMutex
	writeMu       sync.Mutex // Protects concurrent writes
	onMessage     func([]byte)
	closed        bool
	dialContextFn DialContextFunc // Custom dial function (nil = default)
}

// NewWebSocketChannel creates a new WebSocket channel
func NewWebSocketChannel() *WebSocketChannel {
	return &WebSocketChannel{}
}

// SetDialContextFn sets a custom dial function for WebSocket connections
func (w *WebSocketChannel) SetDialContextFn(fn DialContextFunc) {
	w.dialContextFn = fn
}

// newDialer creates a websocket.Dialer with optional custom dial function
func (w *WebSocketChannel) newDialer() websocket.Dialer {
	d := websocket.Dialer{
		HandshakeTimeout: 0, // Use context timeout
	}
	if w.dialContextFn != nil {
		d.NetDialContext = w.dialContextFn
	}
	return d
}

// Open establishes a WebSocket connection
func (w *WebSocketChannel) Open(ctx context.Context, url string) error {
	dialer := w.newDialer()

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
	dialer := w.newDialer()

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
			// Mark as closed so SendMessage returns immediately
			w.mu.Lock()
			w.closed = true
			w.mu.Unlock()
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
