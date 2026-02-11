package datachannel

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xtaci/smux"
)

func TestMuxSession_OpenStream(t *testing.T) {
	// Create pipe for testing
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// Create smux sessions
	serverSession, err := smux.Server(server, smux.DefaultConfig())
	require.NoError(t, err)
	defer serverSession.Close()

	clientSession, err := smux.Client(client, smux.DefaultConfig())
	require.NoError(t, err)
	defer clientSession.Close()

	// Server accepts stream in goroutine
	var serverStream *smux.Stream
	var acceptErr error
	done := make(chan struct{})
	go func() {
		serverStream, acceptErr = serverSession.AcceptStream()
		close(done)
	}()

	// Client opens stream
	clientStream, err := clientSession.OpenStream()
	require.NoError(t, err)
	defer clientStream.Close()

	// Wait for server to accept
	select {
	case <-done:
		require.NoError(t, acceptErr)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for server to accept stream")
	}
	defer serverStream.Close()

	// Test data transfer
	testData := []byte("hello world")
	_, err = clientStream.Write(testData)
	require.NoError(t, err)

	buf := make([]byte, len(testData))
	_, err = io.ReadFull(serverStream, buf)
	require.NoError(t, err)
	assert.Equal(t, testData, buf)
}

func TestMuxSession_MultipleStreams(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	serverSession, err := smux.Server(server, smux.DefaultConfig())
	require.NoError(t, err)
	defer serverSession.Close()

	clientSession, err := smux.Client(client, smux.DefaultConfig())
	require.NoError(t, err)
	defer clientSession.Close()

	// Open multiple streams
	numStreams := 5
	clientStreams := make([]*smux.Stream, numStreams)
	serverStreams := make([]*smux.Stream, numStreams)

	var wg sync.WaitGroup
	wg.Add(numStreams)

	// Accept streams on server
	go func() {
		for i := 0; i < numStreams; i++ {
			stream, err := serverSession.AcceptStream()
			if err != nil {
				return
			}
			serverStreams[i] = stream
			wg.Done()
		}
	}()

	// Open streams on client
	for i := 0; i < numStreams; i++ {
		stream, err := clientSession.OpenStream()
		require.NoError(t, err)
		clientStreams[i] = stream
	}

	// Wait for all streams to be accepted
	wg.Wait()

	// Test data transfer on each stream
	for i := 0; i < numStreams; i++ {
		testData := []byte{byte(i)}
		_, err := clientStreams[i].Write(testData)
		require.NoError(t, err)

		buf := make([]byte, 1)
		_, err = io.ReadFull(serverStreams[i], buf)
		require.NoError(t, err)
		assert.Equal(t, testData, buf)
	}

	// Cleanup
	for i := 0; i < numStreams; i++ {
		clientStreams[i].Close()
		serverStreams[i].Close()
	}
}

func TestMuxSession_ConcurrentReadWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	serverSession, err := smux.Server(server, smux.DefaultConfig())
	require.NoError(t, err)
	defer serverSession.Close()

	clientSession, err := smux.Client(client, smux.DefaultConfig())
	require.NoError(t, err)
	defer clientSession.Close()

	// Accept stream
	done := make(chan struct{})
	var serverStream *smux.Stream
	go func() {
		var err error
		serverStream, err = serverSession.AcceptStream()
		require.NoError(t, err)
		close(done)
	}()

	clientStream, err := clientSession.OpenStream()
	require.NoError(t, err)
	defer clientStream.Close()

	<-done
	defer serverStream.Close()

	// Concurrent read/write
	var wg sync.WaitGroup
	wg.Add(4)

	// Client write
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			clientStream.Write([]byte{byte(i)})
		}
	}()

	// Server read
	go func() {
		defer wg.Done()
		buf := make([]byte, 100)
		io.ReadFull(serverStream, buf)
	}()

	// Server write
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			serverStream.Write([]byte{byte(i)})
		}
	}()

	// Client read
	go func() {
		defer wg.Done()
		buf := make([]byte, 100)
		io.ReadFull(clientStream, buf)
	}()

	wg.Wait()
}

func TestDataChannelConn_ReadWrite(t *testing.T) {
	// Create a pair of connected DataChannelConn
	readChan := make(chan []byte, 10)
	writeChan := make(chan []byte, 10)

	conn := NewDataChannelConn(
		func(data []byte) error {
			writeChan <- data
			return nil
		},
		readChan,
	)
	defer conn.Close()

	// Write test
	testData := []byte("test message")
	n, err := conn.Write(testData)
	require.NoError(t, err)
	assert.Equal(t, len(testData), n)

	// Verify write was sent
	select {
	case data := <-writeChan:
		assert.Equal(t, testData, data)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for write")
	}

	// Read test
	readData := []byte("incoming data")
	go func() {
		readChan <- readData
	}()

	buf := make([]byte, 100)
	n, err = conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, readData, buf[:n])
}

func TestDataChannelConn_SmuxIntegration(t *testing.T) {
	// Create bidirectional channels
	clientToServer := make(chan []byte, 100)
	serverToClient := make(chan []byte, 100)

	clientConn := NewDataChannelConn(
		func(data []byte) error {
			clientToServer <- append([]byte(nil), data...)
			return nil
		},
		serverToClient,
	)
	defer clientConn.Close()

	serverConn := NewDataChannelConn(
		func(data []byte) error {
			serverToClient <- append([]byte(nil), data...)
			return nil
		},
		clientToServer,
	)
	defer serverConn.Close()

	// Create smux sessions
	smuxConfig := smux.DefaultConfig()
	smuxConfig.KeepAliveDisabled = true

	var clientSession *smux.Session
	var serverSession *smux.Session
	var clientErr, serverErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		clientSession, clientErr = smux.Client(clientConn, smuxConfig)
	}()

	go func() {
		defer wg.Done()
		serverSession, serverErr = smux.Server(serverConn, smuxConfig)
	}()

	wg.Wait()
	require.NoError(t, clientErr)
	require.NoError(t, serverErr)
	defer clientSession.Close()
	defer serverSession.Close()

	// Accept stream on server
	done := make(chan struct{})
	var serverStream *smux.Stream
	go func() {
		var err error
		serverStream, err = serverSession.AcceptStream()
		if err == nil {
			close(done)
		}
	}()

	// Open stream on client
	clientStream, err := clientSession.OpenStream()
	require.NoError(t, err)
	defer clientStream.Close()

	// Wait for server to accept
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to accept stream")
	}
	defer serverStream.Close()

	// Test data transfer
	testData := []byte("hello from client")
	_, err = clientStream.Write(testData)
	require.NoError(t, err)

	buf := make([]byte, len(testData))
	_, err = io.ReadFull(serverStream, buf)
	require.NoError(t, err)
	assert.Equal(t, testData, buf)

	// Test reverse direction
	responseData := []byte("hello from server")
	_, err = serverStream.Write(responseData)
	require.NoError(t, err)

	buf = make([]byte, len(responseData))
	_, err = io.ReadFull(clientStream, buf)
	require.NoError(t, err)
	assert.Equal(t, responseData, buf)
}

// TestDataChannelConn_LargeDataTransfer tests transfer of large data (like GraphQL responses or images)
func TestDataChannelConn_LargeDataTransfer(t *testing.T) {
	// Create bidirectional channels with buffer
	clientToServer := make(chan []byte, 10000)
	serverToClient := make(chan []byte, 10000)

	clientConn := NewDataChannelConn(
		func(data []byte) error {
			clientToServer <- append([]byte(nil), data...)
			return nil
		},
		serverToClient,
	)

	serverConn := NewDataChannelConn(
		func(data []byte) error {
			serverToClient <- append([]byte(nil), data...)
			return nil
		},
		clientToServer,
	)

	// Create smux sessions
	smuxConfig := smux.DefaultConfig()
	smuxConfig.KeepAliveDisabled = true
	smuxConfig.MaxReceiveBuffer = 4 * 1024 * 1024 // 4MB buffer

	var clientSession, serverSession *smux.Session
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		var err error
		clientSession, err = smux.Client(clientConn, smuxConfig)
		if err != nil {
			t.Errorf("client session error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		serverSession, err = smux.Server(serverConn, smuxConfig)
		if err != nil {
			t.Errorf("server session error: %v", err)
		}
	}()

	wg.Wait()
	defer clientSession.Close()
	defer serverSession.Close()
	defer clientConn.Close()
	defer serverConn.Close()

	// Accept stream on server
	done := make(chan struct{})
	var serverStream *smux.Stream
	go func() {
		var err error
		serverStream, err = serverSession.AcceptStream()
		if err == nil {
			close(done)
		}
	}()

	// Open stream on client
	clientStream, err := clientSession.OpenStream()
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to accept stream")
	}

	// Test large data transfer (1MB)
	largeData := make([]byte, 1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	// Send from server to client (simulating large response)
	errChan := make(chan error, 1)
	go func() {
		_, err := serverStream.Write(largeData)
		errChan <- err
	}()

	// Receive on client
	received := make([]byte, len(largeData))
	n, err := io.ReadFull(clientStream, received)
	require.NoError(t, err, "read failed after %d bytes", n)

	// Wait for write to complete
	writeErr := <-errChan
	require.NoError(t, writeErr, "write failed")

	assert.Equal(t, largeData, received)

	clientStream.Close()
	serverStream.Close()
}

// TestDataChannelConn_ChunkedTransfer tests data that arrives in small chunks
func TestDataChannelConn_ChunkedTransfer(t *testing.T) {
	readChan := make(chan []byte, 1000)

	conn := NewDataChannelConn(
		func(data []byte) error { return nil },
		readChan,
	)
	defer conn.Close()

	// Send 100KB in 1KB chunks
	totalSize := 100 * 1024
	chunkSize := 1024
	expectedData := make([]byte, totalSize)
	for i := range expectedData {
		expectedData[i] = byte(i % 256)
	}

	// Send chunks in goroutine
	go func() {
		for i := 0; i < totalSize; i += chunkSize {
			end := i + chunkSize
			if end > totalSize {
				end = totalSize
			}
			chunk := make([]byte, end-i)
			copy(chunk, expectedData[i:end])
			readChan <- chunk
		}
	}()

	// Read all data
	received := make([]byte, totalSize)
	totalRead := 0
	for totalRead < totalSize {
		n, err := conn.Read(received[totalRead:])
		require.NoError(t, err)
		totalRead += n
	}

	assert.Equal(t, expectedData, received)
}
