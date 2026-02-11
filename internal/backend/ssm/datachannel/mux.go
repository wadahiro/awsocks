package datachannel

import (
	"errors"
	"net"
	"sync"
	"time"
)

// DataChannelConn wraps DataChannel to implement net.Conn for smux
type DataChannelConn struct {
	writeFunc func([]byte) error
	readChan  <-chan []byte
	readBuf   []byte
	closed    bool
	mu        sync.Mutex
}

// NewDataChannelConn creates a new DataChannelConn
func NewDataChannelConn(writeFunc func([]byte) error, readChan <-chan []byte) *DataChannelConn {
	return &DataChannelConn{
		writeFunc: writeFunc,
		readChan:  readChan,
	}
}

// Read implements net.Conn
func (c *DataChannelConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, errors.New("connection closed")
	}
	c.mu.Unlock()

	// If we have buffered data, use it first
	c.mu.Lock()
	if len(c.readBuf) > 0 {
		n := copy(b, c.readBuf)
		c.readBuf = c.readBuf[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()

	// Wait for new data
	data, ok := <-c.readChan
	if !ok {
		return 0, errors.New("channel closed")
	}

	n := copy(b, data)
	if n < len(data) {
		c.mu.Lock()
		c.readBuf = append(c.readBuf, data[n:]...)
		c.mu.Unlock()
	}

	return n, nil
}

// Write implements net.Conn
func (c *DataChannelConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, errors.New("connection closed")
	}
	c.mu.Unlock()

	if err := c.writeFunc(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close implements net.Conn
func (c *DataChannelConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// LocalAddr implements net.Conn
func (c *DataChannelConn) LocalAddr() net.Addr {
	return &dataChannelAddr{network: "datachannel", address: "local"}
}

// RemoteAddr implements net.Conn
func (c *DataChannelConn) RemoteAddr() net.Addr {
	return &dataChannelAddr{network: "datachannel", address: "remote"}
}

// SetDeadline implements net.Conn
func (c *DataChannelConn) SetDeadline(t time.Time) error {
	return nil // Not implemented
}

// SetReadDeadline implements net.Conn
func (c *DataChannelConn) SetReadDeadline(t time.Time) error {
	return nil // Not implemented
}

// SetWriteDeadline implements net.Conn
func (c *DataChannelConn) SetWriteDeadline(t time.Time) error {
	return nil // Not implemented
}

// dataChannelAddr implements net.Addr
type dataChannelAddr struct {
	network string
	address string
}

func (a *dataChannelAddr) Network() string {
	return a.network
}

func (a *dataChannelAddr) String() string {
	return a.address
}

// Verify DataChannelConn implements net.Conn
var _ net.Conn = (*DataChannelConn)(nil)
