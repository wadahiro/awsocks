package mux

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/protocol"
)

// mockAgent simulates the VM agent side of the protocol.
// It reads MsgConnectDirect messages and responds with MsgConnectAck.
func mockAgent(t *testing.T, conn net.Conn, opts ...mockAgentOption) {
	t.Helper()
	cfg := &mockAgentConfig{}
	for _, o := range opts {
		o(cfg)
	}
	for {
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			return
		}
		switch msg.Type {
		case protocol.MsgConnectDirect:
			if cfg.errorOnConnect {
				errMsg := protocol.NewErrorMessage(msg.ConnID, 1, "connection refused")
				if err := protocol.WriteMessage(conn, errMsg); err != nil {
					return
				}
				continue
			}
			if cfg.connectDelay > 0 {
				time.Sleep(cfg.connectDelay)
			}
			ack := &protocol.Message{
				Type:   protocol.MsgConnectAck,
				ConnID: msg.ConnID,
			}
			if err := protocol.WriteMessage(conn, ack); err != nil {
				return
			}
		case protocol.MsgData:
			if cfg.echoData {
				resp := protocol.NewDataMessage(msg.ConnID, msg.Payload)
				if err := protocol.WriteMessage(conn, resp); err != nil {
					return
				}
			}
		case protocol.MsgClose:
			// ignore
		case protocol.MsgShutdown:
			return
		}
	}
}

type mockAgentConfig struct {
	errorOnConnect bool
	connectDelay   time.Duration
	echoData       bool
}

type mockAgentOption func(*mockAgentConfig)

func withErrorOnConnect() mockAgentOption {
	return func(c *mockAgentConfig) { c.errorOnConnect = true }
}

func withConnectDelay(d time.Duration) mockAgentOption {
	return func(c *mockAgentConfig) { c.connectDelay = d }
}

func withEchoData() mockAgentOption {
	return func(c *mockAgentConfig) { c.echoData = true }
}

func TestAgentMux_Dial_Success(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	go mockAgent(t, agentSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)
	require.NotNil(t, conn)
	conn.Close()
}

func TestAgentMux_Dial_Error(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	go mockAgent(t, agentSide, withErrorOnConnect())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestAgentMux_Dial_Timeout(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	// Agent with long delay
	go mockAgent(t, agentSide, withConnectDelay(10*time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestAgentMux_DataDelivery(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	go mockAgent(t, agentSide, withEchoData())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	// Write data
	testData := []byte("hello from mux")
	n, err := conn.Write(testData)
	require.NoError(t, err)
	assert.Equal(t, len(testData), n)

	// Read echoed data
	buf := make([]byte, 256)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, testData, buf[:n])
}

func TestAgentMux_DataDelivery_PartialRead(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	go mockAgent(t, agentSide, withEchoData())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	// Write large data
	testData := []byte("hello world from mux connection")
	_, err = conn.Write(testData)
	require.NoError(t, err)

	// Read in small buffer (partial read)
	smallBuf := make([]byte, 5)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(smallBuf)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, testData[:5], smallBuf[:n])

	// Read remaining
	remaining := make([]byte, 256)
	n, err = conn.Read(remaining)
	require.NoError(t, err)
	assert.Equal(t, testData[5:], remaining[:n])
}

func TestAgentMux_CloseDelivers_EOF(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	// Custom agent that sends close after ack
	go func() {
		for {
			msg, err := protocol.ReadMessage(agentSide)
			if err != nil {
				return
			}
			if msg.Type == protocol.MsgConnectDirect {
				ack := &protocol.Message{Type: protocol.MsgConnectAck, ConnID: msg.ConnID}
				protocol.WriteMessage(agentSide, ack)
				// Send close after a short delay
				time.Sleep(50 * time.Millisecond)
				closeMsg := protocol.NewCloseMessage(msg.ConnID)
				protocol.WriteMessage(agentSide, closeMsg)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)

	buf := make([]byte, 256)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = conn.Read(buf)
	assert.ErrorIs(t, err, io.EOF)
}

func TestAgentMux_ConcurrentDials_UniqueConnIDs(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	// Track connIDs seen by agent
	var mu sync.Mutex
	seenIDs := make(map[uint32]bool)

	go func() {
		for {
			msg, err := protocol.ReadMessage(agentSide)
			if err != nil {
				return
			}
			if msg.Type == protocol.MsgConnectDirect {
				mu.Lock()
				seenIDs[msg.ConnID] = true
				mu.Unlock()

				ack := &protocol.Message{Type: protocol.MsgConnectAck, ConnID: msg.ConnID}
				if err := protocol.WriteMessage(agentSide, ack); err != nil {
					return
				}
			}
		}
	}()

	const numDials = 100
	var wg sync.WaitGroup
	wg.Add(numDials)

	for i := 0; i < numDials; i++ {
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			conn, err := mux.Dial(ctx, "tcp", fmt.Sprintf("host%d.example.com:443", i))
			if err != nil {
				t.Errorf("Dial %d failed: %v", i, err)
				return
			}
			conn.Close()
		}(i)
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, numDials, len(seenIDs), "all connIDs should be unique")
}

func TestAgentMux_ConcurrentDials_CorrectRouting(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	// Agent that echoes the addr as first data message after ack
	go func() {
		for {
			msg, err := protocol.ReadMessage(agentSide)
			if err != nil {
				return
			}
			switch msg.Type {
			case protocol.MsgConnectDirect:
				// Parse the address from payload
				_, addr, _ := protocol.ParseConnectPayload(msg.Payload)

				ack := &protocol.Message{Type: protocol.MsgConnectAck, ConnID: msg.ConnID}
				if err := protocol.WriteMessage(agentSide, ack); err != nil {
					return
				}

				// Send the addr back as data so the client can verify routing
				dataMsg := protocol.NewDataMessage(msg.ConnID, []byte(addr))
				if err := protocol.WriteMessage(agentSide, dataMsg); err != nil {
					return
				}
			case protocol.MsgClose:
				// ignore
			}
		}
	}()

	const numDials = 10
	var wg sync.WaitGroup
	wg.Add(numDials)

	for i := 0; i < numDials; i++ {
		go func(i int) {
			defer wg.Done()
			addr := fmt.Sprintf("host%d.example.com:443", i)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			conn, err := mux.Dial(ctx, "tcp", addr)
			if err != nil {
				t.Errorf("Dial %d failed: %v", i, err)
				return
			}
			defer conn.Close()

			// Read the echoed addr
			buf := make([]byte, 256)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := conn.Read(buf)
			if err != nil {
				t.Errorf("Read %d failed: %v", i, err)
				return
			}

			received := string(buf[:n])
			assert.Equal(t, addr, received, "connection %d received wrong address", i)
		}(i)
	}

	wg.Wait()
}

func TestAgentMux_SimulateSOCKS5AndAWSAPI(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	// Agent echoes data back with a prefix identifying the conn
	go func() {
		for {
			msg, err := protocol.ReadMessage(agentSide)
			if err != nil {
				return
			}
			switch msg.Type {
			case protocol.MsgConnectDirect:
				ack := &protocol.Message{Type: protocol.MsgConnectAck, ConnID: msg.ConnID}
				if err := protocol.WriteMessage(agentSide, ack); err != nil {
					return
				}
			case protocol.MsgData:
				// Echo with "echo:" prefix
				resp := protocol.NewDataMessage(msg.ConnID, append([]byte("echo:"), msg.Payload...))
				if err := protocol.WriteMessage(agentSide, resp); err != nil {
					return
				}
			case protocol.MsgClose:
				// ignore
			}
		}
	}()

	// Simulate SOCKS5 (vm-direct) goroutine
	socks5Done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		conn, err := mux.Dial(ctx, "tcp", "socks5-target.example.com:80")
		if err != nil {
			socks5Done <- err
			return
		}
		defer conn.Close()

		for i := 0; i < 5; i++ {
			data := fmt.Sprintf("socks5-msg-%d", i)
			if _, err := conn.Write([]byte(data)); err != nil {
				socks5Done <- err
				return
			}

			buf := make([]byte, 256)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := conn.Read(buf)
			if err != nil {
				socks5Done <- err
				return
			}

			expected := "echo:" + data
			if string(buf[:n]) != expected {
				socks5Done <- fmt.Errorf("socks5: expected %q, got %q", expected, string(buf[:n]))
				return
			}
		}
		socks5Done <- nil
	}()

	// Simulate AWS API (vsock dialer) goroutine
	awsapiDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		conn, err := mux.Dial(ctx, "tcp", "ec2.us-east-1.amazonaws.com:443")
		if err != nil {
			awsapiDone <- err
			return
		}
		defer conn.Close()

		for i := 0; i < 5; i++ {
			data := fmt.Sprintf("awsapi-msg-%d", i)
			if _, err := conn.Write([]byte(data)); err != nil {
				awsapiDone <- err
				return
			}

			buf := make([]byte, 256)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := conn.Read(buf)
			if err != nil {
				awsapiDone <- err
				return
			}

			expected := "echo:" + data
			if string(buf[:n]) != expected {
				awsapiDone <- fmt.Errorf("awsapi: expected %q, got %q", expected, string(buf[:n]))
				return
			}
		}
		awsapiDone <- nil
	}()

	require.NoError(t, <-socks5Done, "SOCKS5 goroutine should succeed")
	require.NoError(t, <-awsapiDone, "AWS API goroutine should succeed")
}

// TestAgentMux_DataDelivery_NoDropWhenBufferFull is a regression test for
// silent data loss: handleData used to drop a frame with only a Warn log
// when a conn's readBuf (256 slots) was full, permanently corrupting that
// stream's byte sequence while every other conn kept working. Sending more
// than the buffer size before the client reads any of it must not lose data.
func TestAgentMux_DataDelivery_NoDropWhenBufferFull(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	connIDCh := make(chan uint32, 1)
	go func() {
		for {
			msg, err := protocol.ReadMessage(agentSide)
			if err != nil {
				return
			}
			if msg.Type == protocol.MsgConnectDirect {
				ack := &protocol.Message{Type: protocol.MsgConnectAck, ConnID: msg.ConnID}
				if err := protocol.WriteMessage(agentSide, ack); err != nil {
					return
				}
				connIDCh <- msg.ConnID
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	connID := <-connIDCh

	// Send more data messages than readBuf's capacity (256) with nobody
	// reading yet, so the send loop must fill (and block on) the buffer
	// before the client below starts draining it.
	const numMsgs = 300
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		for i := 0; i < numMsgs; i++ {
			data := []byte(fmt.Sprintf("msg-%03d", i))
			dataMsg := protocol.NewDataMessage(connID, data)
			if err := protocol.WriteMessage(agentSide, dataMsg); err != nil {
				return
			}
		}
	}()

	// Give the send loop time to fill readBuf and block on the 257th+ message
	// (handleData's old behavior: drop it silently instead of blocking).
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < numMsgs; i++ {
		expected := fmt.Sprintf("msg-%03d", i)
		buf := make([]byte, len(expected))
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err := io.ReadFull(conn, buf)
		require.NoError(t, err, "reading message %d", i)
		assert.Equal(t, expected, string(buf), "message %d corrupted or lost", i)
	}

	<-sendDone
}

// TestAgentMux_ReadLoopDeath_PropagatesToEstablishedConns is a regression
// test: when readLoop exits (e.g. the underlying vsock conn breaks), it used
// to only fail pending Dials, leaving already-established MuxConns with no
// signal that their transport died. Their blocked Read would sit for the
// full hardcoded 1-minute timeout instead of failing promptly.
func TestAgentMux_ReadLoopDeath_PropagatesToEstablishedConns(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	go mockAgent(t, agentSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	// Break the transport out from under the mux, simulating the vsock conn
	// dying. readLoop's next ReadMessage call returns an error and exits.
	agentSide.Close()

	readErrCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := conn.Read(buf)
		readErrCh <- err
	}()

	select {
	case err := <-readErrCh:
		require.Error(t, err, "conn.Read should fail once the mux's transport dies")
	case <-time.After(5 * time.Second):
		t.Fatal("conn.Read did not return promptly after readLoop died - it will hang until the 1-minute timeout")
	}
}

// TestAgentMux_PingTimeout_DeclaresMuxDead is a regression test for the
// "vsock open but wedged" case: the underlying conn never returns a read
// error (so readLoop keeps running and RTT/keepalive-style traffic on other
// logical conns can keep flowing), yet the agent has stopped responding.
// Without an active health check, established conns would have no way to
// detect this and would sit until their own 1-minute per-conn timeout.
func TestAgentMux_PingTimeout_DeclaresMuxDead(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide, withPingInterval(50*time.Millisecond), withPongTimeout(200*time.Millisecond))
	defer mux.Close()

	// Agent that acks connects but never answers Ping - simulating a wedged
	// agent side (vsock conn itself stays open).
	go func() {
		for {
			msg, err := protocol.ReadMessage(agentSide)
			if err != nil {
				return
			}
			if msg.Type == protocol.MsgConnectDirect {
				ack := &protocol.Message{Type: protocol.MsgConnectAck, ConnID: msg.ConnID}
				if err := protocol.WriteMessage(agentSide, ack); err != nil {
					return
				}
			}
			// MsgPing intentionally ignored: no MsgPong sent back.
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	readErrCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := conn.Read(buf)
		readErrCh <- err
	}()

	select {
	case err := <-readErrCh:
		require.Error(t, err, "conn.Read should fail once ping health-check declares the mux dead")
	case <-time.After(3 * time.Second):
		t.Fatal("conn.Read did not return after ping timeout - mux health check did not fire")
	}

	// A dead mux must also reject new Dials immediately.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer dialCancel()
	_, err = mux.Dial(dialCtx, "tcp", "other.example.com:443")
	assert.Error(t, err)
}

// failWriteConn wraps a net.Conn and fails every Write once armed, while
// still allowing Close/Read through to the underlying conn.
type failWriteConn struct {
	net.Conn
	armed atomic.Bool
}

func (c *failWriteConn) Write(b []byte) (int, error) {
	if c.armed.Load() {
		return 0, fmt.Errorf("simulated write failure")
	}
	return c.Conn.Write(b)
}

// TestAgentMux_PingWriteError_StillDeclaresMuxDead is a regression test: a
// single failed Ping write used to just `return` from pingLoop, permanently
// disabling the health check for the rest of the process even though
// readLoop's blocking ReadMessage may never itself return (a wedged
// transport can keep accepting reads with no data forever). A write error
// on the ping is evidence of death on its own and must tear the mux down,
// not merely give up monitoring.
func TestAgentMux_PingWriteError_StillDeclaresMuxDead(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()

	wrapped := &failWriteConn{Conn: muxSide}

	mux := NewAgentMux(wrapped, withPingInterval(50*time.Millisecond), withPongTimeout(200*time.Millisecond))
	defer mux.Close()

	go mockAgent(t, agentSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)
	defer conn.Close()

	// Arm the write failure only after the conn is established, so the
	// failure hits a ping write, not the Dial's ConnectDirect write.
	wrapped.armed.Store(true)

	readErrCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := conn.Read(buf)
		readErrCh <- err
	}()

	select {
	case err := <-readErrCh:
		require.Error(t, err, "conn.Read should fail once a ping write error declares the mux dead")
	case <-time.After(3 * time.Second):
		t.Fatal("conn.Read did not return after ping write failure - pingLoop stopped monitoring instead of declaring the mux dead")
	}
}

func TestAgentMux_SendShutdown(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	shutdownReceived := make(chan bool, 1)
	go func() {
		for {
			msg, err := protocol.ReadMessage(agentSide)
			if err != nil {
				return
			}
			if msg.Type == protocol.MsgShutdown {
				shutdownReceived <- true
				return
			}
		}
	}()

	err := mux.SendShutdown()
	require.NoError(t, err)

	select {
	case <-shutdownReceived:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown message not received")
	}
}

func TestAgentMux_LogHandler(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	var receivedLog *protocol.LogPayload
	var logMu sync.Mutex

	mux := NewAgentMux(muxSide, WithLogHandler(func(payload *protocol.LogPayload) {
		logMu.Lock()
		receivedLog = payload
		logMu.Unlock()
	}))
	defer mux.Close()

	// Send a log message from agent side
	logMsg := protocol.NewLogMessage("info", "test log message")
	err := protocol.WriteMessage(agentSide, logMsg)
	require.NoError(t, err)

	// Wait for log to be processed
	time.Sleep(200 * time.Millisecond)

	logMu.Lock()
	defer logMu.Unlock()
	require.NotNil(t, receivedLog)
	assert.Equal(t, "info", receivedLog.Level)
	assert.Equal(t, "test log message", receivedLog.Message)
}

func TestAgentMux_Close_StopsReadLoop(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()

	mux := NewAgentMux(muxSide)

	// Close the mux
	err := mux.Close()
	require.NoError(t, err)

	// Dial should fail after close
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	assert.Nil(t, conn)
	assert.Error(t, err)
}

func TestAgentMux_MuxConn_WriteAfterClose(t *testing.T) {
	agentSide, muxSide := net.Pipe()
	defer agentSide.Close()
	defer muxSide.Close()

	mux := NewAgentMux(muxSide)
	defer mux.Close()

	go mockAgent(t, agentSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := mux.Dial(ctx, "tcp", "example.com:443")
	require.NoError(t, err)

	conn.Close()

	_, err = conn.Write([]byte("data"))
	assert.Error(t, err)
	assert.Equal(t, io.ErrClosedPipe, err)
}
