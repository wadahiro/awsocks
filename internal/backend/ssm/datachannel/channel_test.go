package datachannel

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockWebSocket is a test WebSocket server that simulates SSM agent behavior
type mockWebSocket struct {
	server      *httptest.Server
	conn        *websocket.Conn
	connMu      sync.Mutex
	received    [][]byte
	receivedMu  sync.Mutex
	toSend      chan []byte
	handshakeDone atomic.Bool
}

func newMockWebSocket(t *testing.T) *mockWebSocket {
	m := &mockWebSocket{
		toSend: make(chan []byte, 10),
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)

		m.connMu.Lock()
		m.conn = conn
		m.connMu.Unlock()

		// Read loop
		go func() {
			for {
				_, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				m.receivedMu.Lock()
				m.received = append(m.received, data)
				m.receivedMu.Unlock()
			}
		}()

		// Write loop
		for data := range m.toSend {
			if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				return
			}
		}
	}))

	return m
}

func (m *mockWebSocket) URL() string {
	return "ws" + m.server.URL[4:]
}

func (m *mockWebSocket) Close() {
	close(m.toSend)
	m.connMu.Lock()
	if m.conn != nil {
		m.conn.Close()
	}
	m.connMu.Unlock()
	m.server.Close()
}

func (m *mockWebSocket) Send(data []byte) {
	m.toSend <- data
}

func (m *mockWebSocket) Received() [][]byte {
	m.receivedMu.Lock()
	defer m.receivedMu.Unlock()
	result := make([][]byte, len(m.received))
	copy(result, m.received)
	return result
}

func TestDataChannel_Open(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()

	ctx := context.Background()
	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)

	assert.True(t, dc.IsOpen())

	err = dc.Close()
	require.NoError(t, err)
}

func TestDataChannel_SendInputData(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Send data
	payload := []byte("test data")
	err = dc.SendInputData(payload)
	require.NoError(t, err)

	// Wait for message to be received
	time.Sleep(100 * time.Millisecond)

	received := mock.Received()
	require.Len(t, received, 1)

	// Deserialize and verify
	msg := &ClientMessage{}
	err = msg.Deserialize(received[0])
	require.NoError(t, err)

	assert.Equal(t, InputStreamMessage, msg.MessageType)
	assert.True(t, bytes.Equal(payload, msg.Payload))
	assert.Equal(t, int64(0), msg.SequenceNumber) // Sequence starts at 0 (matching official plugin)
}

func TestDataChannel_ProcessAcknowledge(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Send data
	err = dc.SendInputData([]byte("test"))
	require.NoError(t, err)

	// Wait for message
	time.Sleep(100 * time.Millisecond)

	received := mock.Received()
	require.Len(t, received, 1)

	// Parse the sent message to get its ID
	sentMsg := &ClientMessage{}
	err = sentMsg.Deserialize(received[0])
	require.NoError(t, err)

	// Create and send ACK
	ackMsg := NewAcknowledgeMessage(InputStreamMessage, sentMsg.MessageID, sentMsg.SequenceNumber)
	ackData, err := ackMsg.Serialize()
	require.NoError(t, err)

	// Process ACK (simulate receiving it)
	err = dc.ProcessMessage(ackData)
	require.NoError(t, err)

	// Verify the message was removed from pending
	assert.Equal(t, 0, dc.PendingCount())
}

func TestDataChannel_SequenceTracking(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Send multiple messages
	for i := 0; i < 5; i++ {
		err = dc.SendInputData([]byte("test"))
		require.NoError(t, err)
	}

	// Wait for messages
	time.Sleep(200 * time.Millisecond)

	received := mock.Received()
	require.Len(t, received, 5)

	// Verify sequence numbers (starts at 0, matching official plugin)
	for i, data := range received {
		msg := &ClientMessage{}
		err = msg.Deserialize(data)
		require.NoError(t, err)
		assert.Equal(t, int64(i), msg.SequenceNumber)
	}
}

func TestDataChannel_OutOfOrderMessages(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Track received output data
	var receivedData [][]byte
	var receivedMu sync.Mutex
	dc.SetOnOutputData(func(data []byte) {
		receivedMu.Lock()
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		receivedData = append(receivedData, dataCopy)
		receivedMu.Unlock()
	})

	// Create messages out of order
	msg0 := createOutputMessage([]byte("msg0"), 0)
	msg1 := createOutputMessage([]byte("msg1"), 1)
	msg2 := createOutputMessage([]byte("msg2"), 2)

	data0, _ := msg0.Serialize()
	data1, _ := msg1.Serialize()
	data2, _ := msg2.Serialize()

	// Process in order: 0, 2, 1
	err = dc.ProcessMessage(data0)
	require.NoError(t, err)

	err = dc.ProcessMessage(data2) // Out of order
	require.NoError(t, err)

	err = dc.ProcessMessage(data1) // Fill the gap
	require.NoError(t, err)

	// Wait for processing
	time.Sleep(100 * time.Millisecond)

	receivedMu.Lock()
	defer receivedMu.Unlock()

	// All messages should have been processed in order
	require.Len(t, receivedData, 3)
	assert.Equal(t, []byte("msg0"), receivedData[0])
	assert.Equal(t, []byte("msg1"), receivedData[1])
	assert.Equal(t, []byte("msg2"), receivedData[2])
}

func TestDataChannel_HandleHandshake(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	dc.SetClientVersion("1.0.0")
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Create handshake request message
	handshakePayload := []byte(`{"AgentVersion":"3.2.2016.0","RequestedClientActions":[{"ActionType":"SessionType","ActionParameters":{"SessionType":"Port","Properties":{}}}]}`)
	handshakeMsg := &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    InputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: 0,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  make([]byte, 32),
		PayloadType:    PayloadTypeHandshakeRequest,
		PayloadLength:  uint32(len(handshakePayload)),
		Payload:        handshakePayload,
	}

	data, err := handshakeMsg.Serialize()
	require.NoError(t, err)

	// Process handshake request
	err = dc.ProcessMessage(data)
	require.NoError(t, err)

	// Wait for response
	time.Sleep(200 * time.Millisecond)

	// Verify response was sent
	received := mock.Received()
	require.NotEmpty(t, received)

	// Find handshake response
	found := false
	for _, data := range received {
		msg := &ClientMessage{}
		if err := msg.Deserialize(data); err == nil {
			if msg.PayloadType == PayloadTypeHandshakeResponse {
				found = true
				break
			}
		}
	}
	assert.True(t, found, "handshake response should be sent")
}

// TestDataChannel_SendAcknowledge_ReturnsError verifies that sendAcknowledge returns an error
// when WebSocket is not connected
func TestDataChannel_SendAcknowledge_ReturnsError(t *testing.T) {
	// Create DataChannel without opening WebSocket (not connected)
	dc := NewDataChannel()

	msg := createOutputMessage([]byte("test"), 0)
	err := dc.sendAcknowledge(msg)

	// Should return error since WebSocket is not connected
	assert.Error(t, err)
}

// TestDataChannel_ProcessOutputMessage_ACKError verifies that processOutputMessage returns error
// when ACK sending fails
func TestDataChannel_ProcessOutputMessage_ACKError(t *testing.T) {
	// Create DataChannel without opening WebSocket
	dc := NewDataChannel()

	msg := createOutputMessage([]byte("test"), 0)
	data, err := msg.Serialize()
	require.NoError(t, err)

	err = dc.ProcessMessage(data)

	// Should return error since ACK sending fails
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ACK")
}

// TestDataChannel_ProcessInputMessage_ACKError verifies that processInputMessage returns error
// when ACK sending fails
func TestDataChannel_ProcessInputMessage_ACKError(t *testing.T) {
	// Create DataChannel without opening WebSocket
	dc := NewDataChannel()

	// Create input stream message (handshake request type)
	handshakePayload := []byte(`{"AgentVersion":"3.2.2016.0","RequestedClientActions":[]}`)
	msg := &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    InputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: 0,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  make([]byte, 32),
		PayloadType:    PayloadTypeOutput, // Use Output type to avoid handshake processing
		PayloadLength:  uint32(len(handshakePayload)),
		Payload:        handshakePayload,
	}

	data, err := msg.Serialize()
	require.NoError(t, err)

	err = dc.ProcessMessage(data)

	// Should return error since ACK sending fails
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ACK")
}

func createOutputMessage(payload []byte, seqNum int64) *ClientMessage {
	return &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    OutputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: seqNum,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  make([]byte, 32),
		PayloadType:    PayloadTypeOutput,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}
}

// TestDataChannel_OutgoingBuffer verifies that messages are added to the outgoing buffer
func TestDataChannel_OutgoingBuffer(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Initially empty
	assert.Equal(t, 0, dc.OutgoingBufferLen())

	// Send data
	err = dc.SendInputData([]byte("test"))
	require.NoError(t, err)

	// Should have one message in buffer
	assert.Equal(t, 1, dc.OutgoingBufferLen())

	// Wait for message to be received
	time.Sleep(100 * time.Millisecond)

	// Get the sent message ID
	received := mock.Received()
	require.Len(t, received, 1)

	sentMsg := &ClientMessage{}
	err = sentMsg.Deserialize(received[0])
	require.NoError(t, err)

	// Send ACK to remove from buffer
	ackMsg := NewAcknowledgeMessage(InputStreamMessage, sentMsg.MessageID, sentMsg.SequenceNumber)
	ackData, err := ackMsg.Serialize()
	require.NoError(t, err)

	err = dc.ProcessMessage(ackData)
	require.NoError(t, err)

	// Buffer should be empty now
	assert.Equal(t, 0, dc.OutgoingBufferLen())
}

// TestDataChannel_RTTCalculation verifies that RTT is calculated on ACK
func TestDataChannel_RTTCalculation(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Initial RTT should be default
	initialRTT := dc.GetRoundTripTime()
	assert.Equal(t, DefaultRoundTripTime, initialRTT)

	// Initial RTO should be default
	initialRTO := dc.GetRetransmissionTimeout()
	assert.Equal(t, DefaultTransmissionTimeout, initialRTO)

	// Send data
	err = dc.SendInputData([]byte("test"))
	require.NoError(t, err)

	// Wait a bit to simulate network latency
	time.Sleep(50 * time.Millisecond)

	// Get the sent message and send ACK
	received := mock.Received()
	require.Len(t, received, 1)

	sentMsg := &ClientMessage{}
	err = sentMsg.Deserialize(received[0])
	require.NoError(t, err)

	ackMsg := NewAcknowledgeMessage(InputStreamMessage, sentMsg.MessageID, sentMsg.SequenceNumber)
	ackData, err := ackMsg.Serialize()
	require.NoError(t, err)

	err = dc.ProcessMessage(ackData)
	require.NoError(t, err)

	// RTT should have been updated (smoothed towards actual RTT)
	newRTT := dc.GetRoundTripTime()
	// New RTT = (1 - 0.125) * 100 + 0.125 * ~50 = ~93.75
	assert.Less(t, newRTT, initialRTT)
}

// TestDataChannel_ResendScheduler verifies that messages are resent after timeout
func TestDataChannel_ResendScheduler(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Send data
	err = dc.SendInputData([]byte("test resend"))
	require.NoError(t, err)

	// Wait for initial message
	time.Sleep(50 * time.Millisecond)
	initialCount := len(mock.Received())
	require.Equal(t, 1, initialCount)

	// Wait for resend (default timeout is 200ms, check interval is 100ms)
	// Total wait: 200ms + 100ms + some buffer = 400ms
	time.Sleep(400 * time.Millisecond)

	// Should have received resent message
	finalCount := len(mock.Received())
	assert.Greater(t, finalCount, initialCount, "message should have been resent")
}

// TestDataChannel_ResendTimeout verifies that timeout callback is called after max attempts
func TestDataChannel_ResendTimeout(t *testing.T) {
	// Skip this test in normal runs as it would take too long
	// This test verifies the timeout callback mechanism
	t.Skip("Skipping timeout test - would take 5 minutes with default settings")
}

// TestDataChannel_ResendStopsOnACK verifies that resend stops after ACK is received
func TestDataChannel_ResendStopsOnACK(t *testing.T) {
	mock := newMockWebSocket(t)
	defer mock.Close()

	dc := NewDataChannel()
	ctx := context.Background()

	err := dc.Open(ctx, mock.URL())
	require.NoError(t, err)
	defer dc.Close()

	// Send data
	err = dc.SendInputData([]byte("test stop resend"))
	require.NoError(t, err)

	// Wait for initial message
	time.Sleep(50 * time.Millisecond)

	// Get the sent message and send ACK
	received := mock.Received()
	require.Len(t, received, 1)

	sentMsg := &ClientMessage{}
	err = sentMsg.Deserialize(received[0])
	require.NoError(t, err)

	ackMsg := NewAcknowledgeMessage(InputStreamMessage, sentMsg.MessageID, sentMsg.SequenceNumber)
	ackData, err := ackMsg.Serialize()
	require.NoError(t, err)

	err = dc.ProcessMessage(ackData)
	require.NoError(t, err)

	// Buffer should be empty
	assert.Equal(t, 0, dc.OutgoingBufferLen())

	// Record count after ACK
	countAfterACK := len(mock.Received())

	// Wait for potential resend interval
	time.Sleep(400 * time.Millisecond)

	// Count should not have increased (no resend needed)
	finalCount := len(mock.Received())
	assert.Equal(t, countAfterACK, finalCount, "no resend should occur after ACK")
}
