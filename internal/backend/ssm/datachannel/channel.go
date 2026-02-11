package datachannel

import (
	"container/list"
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wadahiro/awsocks/internal/log"
)

var dcLogger = log.For(log.ComponentDataChannel)

// StreamingMessage represents a message in the outgoing buffer for resend
type StreamingMessage struct {
	Content        []byte
	SequenceNumber int64
	MessageID      string
	LastSentTime   time.Time
	ResendAttempt  int
}

// DataChannel manages the communication channel with SSM agent
type DataChannel struct {
	ws            *WebSocketChannel
	mu            sync.RWMutex

	// Sequence tracking
	sendSeqNum           int64
	expectedSeqNum       int64
	incomingBuffer       map[int64]*ClientMessage
	pendingMessages      map[string]*pendingMessage

	// Handshake
	clientVersion        string
	handshakeProcessor   *HandshakeProcessor
	handshakeComplete    bool

	// Callbacks
	onOutputData         func([]byte)
	onHandshakeComplete  func()
	onError              func(error)
	onDisconnect         func()
	onResendTimeout      func()

	// State
	closed               bool

	// Retransmission mechanism
	outgoingBuffer            *list.List
	outgoingMu                sync.Mutex
	roundTripTime             float64 // in milliseconds
	roundTripTimeVariation    float64 // in milliseconds
	retransmissionTimeout     time.Duration
	resendStopCh              chan struct{}
}

// pendingMessage tracks a sent message awaiting acknowledgment
type pendingMessage struct {
	message   *ClientMessage
	sentAt    time.Time
	attempts  int
}

// NewDataChannel creates a new DataChannel
func NewDataChannel() *DataChannel {
	return &DataChannel{
		ws:                      NewWebSocketChannel(),
		incomingBuffer:          make(map[int64]*ClientMessage),
		pendingMessages:         make(map[string]*pendingMessage),
		clientVersion:           "1.0.0",
		outgoingBuffer:          list.New(),
		roundTripTime:           DefaultRoundTripTime,
		roundTripTimeVariation:  DefaultRoundTripTimeVariation,
		retransmissionTimeout:   DefaultTransmissionTimeout,
		resendStopCh:            make(chan struct{}),
	}
}

// Open opens the data channel
func (d *DataChannel) Open(ctx context.Context, url string) error {
	d.mu.Lock()
	d.closed = false
	d.handshakeProcessor = NewHandshakeProcessor(d.clientVersion)
	d.resendStopCh = make(chan struct{})
	d.mu.Unlock()

	if err := d.ws.Open(ctx, url); err != nil {
		return err
	}

	// Set up message handler
	d.ws.SetOnMessage(func(data []byte) {
		if err := d.ProcessMessage(data); err != nil {
			d.mu.RLock()
			onError := d.onError
			d.mu.RUnlock()
			if onError != nil {
				onError(err)
			}
		}
	})

	// Start receiving in a goroutine that notifies on disconnect
	go func() {
		d.ws.StartReceiving()
		// When StartReceiving returns, the WebSocket connection is closed
		d.mu.RLock()
		onDisconnect := d.onDisconnect
		closed := d.closed
		d.mu.RUnlock()
		// Only call onDisconnect if not intentionally closed
		if onDisconnect != nil && !closed {
			onDisconnect()
		}
	}()

	// Start resend scheduler
	d.startResendScheduler()

	return nil
}

// Close closes the data channel
func (d *DataChannel) Close() error {
	d.mu.Lock()
	d.closed = true
	// Stop resend scheduler
	select {
	case <-d.resendStopCh:
		// Already closed
	default:
		close(d.resendStopCh)
	}
	d.mu.Unlock()

	return d.ws.Close()
}

// IsOpen returns whether the channel is open
func (d *DataChannel) IsOpen() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return !d.closed && d.ws.IsOpen()
}

// SetClientVersion sets the client version for handshake
func (d *DataChannel) SetClientVersion(version string) {
	d.mu.Lock()
	d.clientVersion = version
	d.mu.Unlock()
}

// SetOnOutputData sets the callback for output data
func (d *DataChannel) SetOnOutputData(handler func([]byte)) {
	d.mu.Lock()
	d.onOutputData = handler
	d.mu.Unlock()
}

// SetOnHandshakeComplete sets the callback for handshake completion
func (d *DataChannel) SetOnHandshakeComplete(handler func()) {
	d.mu.Lock()
	d.onHandshakeComplete = handler
	d.mu.Unlock()
}

// SetOnError sets the callback for errors
func (d *DataChannel) SetOnError(handler func(error)) {
	d.mu.Lock()
	d.onError = handler
	d.mu.Unlock()
}

// SetOnDisconnect sets the callback for disconnection
func (d *DataChannel) SetOnDisconnect(handler func()) {
	d.mu.Lock()
	d.onDisconnect = handler
	d.mu.Unlock()
}

// SetOnResendTimeout sets the callback for resend timeout
func (d *DataChannel) SetOnResendTimeout(handler func()) {
	d.mu.Lock()
	d.onResendTimeout = handler
	d.mu.Unlock()
}

// Flags for messages
const (
	FlagData uint64 = 0
	FlagSyn  uint64 = 1
	FlagFin  uint64 = 2
	FlagAck  uint64 = 3
)

// SendInputData sends data to the agent
func (d *DataChannel) SendInputData(payload []byte) error {
	d.mu.Lock()
	// Use current sequence number, then increment (starts at 0, matching official plugin)
	seqNum := d.sendSeqNum
	d.sendSeqNum++
	d.mu.Unlock()

	msg := NewInputStreamMessage(payload, seqNum)
	// Flags = 0 (Data) - official plugin doesn't use SYN flag
	msg.Flags = FlagData

	return d.sendMessage(msg)
}

// sendMessage serializes and sends a message
func (d *DataChannel) sendMessage(msg *ClientMessage) error {
	data, err := msg.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	// Track pending message for ACK and resend
	if msg.MessageType == InputStreamMessage {
		d.mu.Lock()
		d.pendingMessages[msg.MessageID.String()] = &pendingMessage{
			message:  msg,
			sentAt:   time.Now(),
			attempts: 1,
		}
		d.mu.Unlock()

		// Add to outgoing buffer for resend scheduling
		d.addToOutgoingBuffer(StreamingMessage{
			Content:        data,
			SequenceNumber: msg.SequenceNumber,
			MessageID:      msg.MessageID.String(),
			LastSentTime:   time.Now(),
			ResendAttempt:  0,
		})
	}

	return d.ws.SendMessage(data)
}

// addToOutgoingBuffer adds a message to the outgoing buffer for resend
func (d *DataChannel) addToOutgoingBuffer(msg StreamingMessage) {
	d.outgoingMu.Lock()
	defer d.outgoingMu.Unlock()

	// Remove oldest if capacity exceeded
	if d.outgoingBuffer.Len() >= OutgoingMessageBufferCapacity {
		front := d.outgoingBuffer.Front()
		if front != nil {
			d.outgoingBuffer.Remove(front)
		}
	}

	d.outgoingBuffer.PushBack(msg)
}

// ProcessMessage processes an incoming message
func (d *DataChannel) ProcessMessage(data []byte) error {
	msg := &ClientMessage{}
	if err := msg.Deserialize(data); err != nil {
		return fmt.Errorf("failed to deserialize message: %w", err)
	}

	switch msg.MessageType {
	case AcknowledgeMessage:
		return d.processAcknowledge(msg)
	case OutputStreamMessage:
		return d.processOutputMessage(msg)
	case InputStreamMessage:
		// Input from agent (handshake, etc.)
		return d.processInputMessage(msg)
	case ChannelClosedMessage:
		dcLogger.Debug("Received ChannelClosedMessage from SSM, closing connection", "seq", msg.SequenceNumber, "payload", string(msg.Payload))
		return d.Close()
	default:
		// Unknown message type - ignore
		return nil
	}
}

// processAcknowledge handles acknowledge messages
func (d *DataChannel) processAcknowledge(msg *ClientMessage) error {
	ack := &AcknowledgeContent{}
	if err := ack.Unmarshal(msg.Payload); err != nil {
		return fmt.Errorf("failed to parse acknowledge: %w", err)
	}

	d.mu.Lock()
	delete(d.pendingMessages, ack.AcknowledgedMessageID)
	d.mu.Unlock()

	// Remove from outgoing buffer and calculate RTT
	d.outgoingMu.Lock()
	for e := d.outgoingBuffer.Front(); e != nil; e = e.Next() {
		streamMsg := e.Value.(StreamingMessage)
		if streamMsg.MessageID == ack.AcknowledgedMessageID {
			// Only calculate RTT if this is not a resent message
			if streamMsg.ResendAttempt == 0 {
				d.calculateRetransmissionTimeout(streamMsg)
			}
			d.outgoingBuffer.Remove(e)
			break
		}
	}
	d.outgoingMu.Unlock()

	return nil
}

// processOutputMessage handles output stream messages
func (d *DataChannel) processOutputMessage(msg *ClientMessage) error {
	// Send ACK synchronously (critical for SSM stability)
	if err := d.sendAcknowledge(msg); err != nil {
		return fmt.Errorf("failed to send ACK for output seq %d: %w", msg.SequenceNumber, err)
	}

	// Handle based on payload type
	switch msg.PayloadType {
	case PayloadTypeHandshakeRequest:
		return d.handleHandshakeRequest(msg)
	case PayloadTypeHandshakeComplete:
		return d.handleHandshakeComplete(msg)
	case PayloadTypeOutput:
		d.mu.Lock()
		defer d.mu.Unlock()

		// Handle sequence ordering
		if msg.SequenceNumber == d.expectedSeqNum {
			// In order - process immediately
			d.deliverMessage(msg)
			d.expectedSeqNum++

			// Process any buffered messages
			for {
				buffered, ok := d.incomingBuffer[d.expectedSeqNum]
				if !ok {
					break
				}
				d.deliverMessage(buffered)
				delete(d.incomingBuffer, d.expectedSeqNum)
				d.expectedSeqNum++
			}
		} else if msg.SequenceNumber > d.expectedSeqNum {
			// Out of order - buffer it
			d.incomingBuffer[msg.SequenceNumber] = msg
		}
		// Duplicate (seq < expected) - ignore
	default:
		// Unhandled payload type - ignore
	}

	return nil
}

// processInputMessage handles input stream messages from agent
func (d *DataChannel) processInputMessage(msg *ClientMessage) error {
	// Send ACK synchronously (critical for SSM stability)
	if err := d.sendAcknowledge(msg); err != nil {
		return fmt.Errorf("failed to send ACK for input seq %d: %w", msg.SequenceNumber, err)
	}

	switch msg.PayloadType {
	case PayloadTypeHandshakeRequest:
		return d.handleHandshakeRequest(msg)
	case PayloadTypeHandshakeComplete:
		return d.handleHandshakeComplete(msg)
	default:
		// Regular output data
		d.mu.Lock()
		onOutput := d.onOutputData
		d.mu.Unlock()
		if onOutput != nil {
			onOutput(msg.Payload)
		}
	}

	return nil
}

// handleHandshakeRequest processes handshake request from agent
func (d *DataChannel) handleHandshakeRequest(msg *ClientMessage) error {
	d.mu.RLock()
	processor := d.handshakeProcessor
	d.mu.RUnlock()

	resp, err := processor.ProcessHandshake(msg.Payload)
	if err != nil {
		return fmt.Errorf("failed to process handshake: %w", err)
	}

	respData, err := resp.Build()
	if err != nil {
		return fmt.Errorf("failed to build handshake response: %w", err)
	}

	return d.sendHandshakeResponse(respData)
}

// sendHandshakeResponse sends the handshake response
func (d *DataChannel) sendHandshakeResponse(payload []byte) error {
	d.mu.Lock()
	seqNum := d.sendSeqNum
	d.sendSeqNum++
	d.mu.Unlock()

	payloadHash := sha256.Sum256(payload)
	msg := &ClientMessage{
		HeaderLength:   ClientMessageHeaderLength,
		MessageType:    InputStreamMessage,
		SchemaVersion:  1,
		CreatedDate:    uint64(time.Now().UnixMilli()),
		SequenceNumber: seqNum,
		Flags:          0,
		MessageID:      uuid.New(),
		PayloadDigest:  payloadHash[:],
		PayloadType:    PayloadTypeHandshakeResponse,
		PayloadLength:  uint32(len(payload)),
		Payload:        payload,
	}

	return d.sendMessage(msg)
}

// handleHandshakeComplete processes handshake complete from agent
func (d *DataChannel) handleHandshakeComplete(msg *ClientMessage) error {
	d.mu.Lock()
	d.handshakeComplete = true
	// Reset expected sequence number after handshake
	// The next data message will have sequence number after handshake messages
	d.expectedSeqNum = msg.SequenceNumber + 1
	onComplete := d.onHandshakeComplete
	d.mu.Unlock()

	if onComplete != nil {
		onComplete()
	}

	return nil
}

// sendAcknowledge sends an acknowledge message
func (d *DataChannel) sendAcknowledge(msg *ClientMessage) error {
	ack := NewAcknowledgeMessage(msg.MessageType, msg.MessageID, msg.SequenceNumber)
	data, err := ack.Serialize()
	if err != nil {
		return fmt.Errorf("failed to serialize ACK: %w", err)
	}
	if err := d.ws.SendMessage(data); err != nil {
		return fmt.Errorf("failed to send ACK: %w", err)
	}
	return nil
}

// deliverMessage delivers a message to the output handler
func (d *DataChannel) deliverMessage(msg *ClientMessage) {
	if d.onOutputData != nil && msg.PayloadType == PayloadTypeOutput {
		d.onOutputData(msg.Payload)
	}
}

// PendingCount returns the number of pending messages
func (d *DataChannel) PendingCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.pendingMessages)
}

// IsHandshakeComplete returns whether handshake is complete
func (d *DataChannel) IsHandshakeComplete() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.handshakeComplete
}

// GetWebSocket returns the underlying WebSocket channel (for mux integration)
func (d *DataChannel) GetWebSocket() *WebSocketChannel {
	return d.ws
}

// SendRaw sends raw bytes over the WebSocket (for initial handshake)
func (d *DataChannel) SendRaw(data []byte) error {
	return d.ws.SendMessage(data)
}

// SendJSON sends JSON data over the WebSocket (for OpenDataChannel)
func (d *DataChannel) SendJSON(v any) error {
	return d.ws.SendJSON(v)
}

// calculateRetransmissionTimeout updates RTT and RTO based on Jacobson-Karels algorithm
// Must be called with outgoingMu held
func (d *DataChannel) calculateRetransmissionTimeout(msg StreamingMessage) {
	newRTT := float64(time.Since(msg.LastSentTime) / time.Millisecond)

	// RTTVAR = (1 - RTTVConstant) * RTTVAR + RTTVConstant * |RTT - newRTT|
	d.roundTripTimeVariation = ((1 - RTTVConstant) * d.roundTripTimeVariation) +
		(RTTVConstant * math.Abs(d.roundTripTime-newRTT))

	// RTT = (1 - RTTConstant) * RTT + RTTConstant * newRTT
	d.roundTripTime = ((1 - RTTConstant) * d.roundTripTime) + (RTTConstant * newRTT)

	// RTO = RTT + max(ClockGranularity, 4 * RTTVAR)
	clockGranularityMs := float64(ClockGranularity / time.Millisecond)
	rto := d.roundTripTime + math.Max(clockGranularityMs, 4*d.roundTripTimeVariation)
	d.retransmissionTimeout = time.Duration(rto) * time.Millisecond

	// Apply upper limit
	if d.retransmissionTimeout > MaxTransmissionTimeout {
		d.retransmissionTimeout = MaxTransmissionTimeout
	}

	dcLogger.Debug("RTT updated",
		"rtt_ms", d.roundTripTime,
		"rttvar_ms", d.roundTripTimeVariation,
		"rto_ms", rto)
}

// startResendScheduler starts the background goroutine that checks for messages needing resend
func (d *DataChannel) startResendScheduler() {
	go func() {
		ticker := time.NewTicker(ResendSleepInterval)
		defer ticker.Stop()

		for {
			select {
			case <-d.resendStopCh:
				return
			case <-ticker.C:
				d.checkAndResend()
			}
		}
	}()
}

// checkAndResend checks the front message and resends if needed
func (d *DataChannel) checkAndResend() {
	d.outgoingMu.Lock()
	front := d.outgoingBuffer.Front()
	if front == nil {
		d.outgoingMu.Unlock()
		return
	}

	msg := front.Value.(StreamingMessage)
	timeout := d.retransmissionTimeout
	d.outgoingMu.Unlock()

	if time.Since(msg.LastSentTime) <= timeout {
		return
	}

	if msg.ResendAttempt >= ResendMaxAttempt {
		dcLogger.Warn("Message resend timeout",
			"seq", msg.SequenceNumber,
			"attempts", msg.ResendAttempt)

		d.mu.RLock()
		onTimeout := d.onResendTimeout
		d.mu.RUnlock()
		if onTimeout != nil {
			onTimeout()
		}
		return
	}

	dcLogger.Debug("Resending message",
		"seq", msg.SequenceNumber,
		"attempt", msg.ResendAttempt+1)

	// Resend the message
	if err := d.ws.SendMessage(msg.Content); err != nil {
		dcLogger.Error("Failed to resend message", "error", err)
		return
	}

	// Update the message state
	d.outgoingMu.Lock()
	if front := d.outgoingBuffer.Front(); front != nil {
		updated := front.Value.(StreamingMessage)
		if updated.MessageID == msg.MessageID {
			updated.ResendAttempt++
			updated.LastSentTime = time.Now()
			front.Value = updated
		}
	}
	d.outgoingMu.Unlock()
}

// OutgoingBufferLen returns the number of messages in the outgoing buffer (for testing)
func (d *DataChannel) OutgoingBufferLen() int {
	d.outgoingMu.Lock()
	defer d.outgoingMu.Unlock()
	return d.outgoingBuffer.Len()
}

// GetRetransmissionTimeout returns the current retransmission timeout (for testing)
func (d *DataChannel) GetRetransmissionTimeout() time.Duration {
	d.outgoingMu.Lock()
	defer d.outgoingMu.Unlock()
	return d.retransmissionTimeout
}

// GetRoundTripTime returns the current RTT in milliseconds (for testing)
func (d *DataChannel) GetRoundTripTime() float64 {
	d.outgoingMu.Lock()
	defer d.outgoingMu.Unlock()
	return d.roundTripTime
}
