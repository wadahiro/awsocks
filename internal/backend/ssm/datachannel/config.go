package datachannel

import "time"

// Retransmission configuration constants
// Based on session-manager-plugin defaults
const (
	// DefaultTransmissionTimeout is the initial retransmission timeout
	DefaultTransmissionTimeout = 200 * time.Millisecond

	// DefaultRoundTripTime is the initial RTT value in milliseconds
	DefaultRoundTripTime = 100.0

	// DefaultRoundTripTimeVariation is the initial RTTVAR value
	DefaultRoundTripTimeVariation = 0.0

	// ResendSleepInterval is how often to check for messages needing resend
	ResendSleepInterval = 100 * time.Millisecond

	// ResendMaxAttempt is the maximum number of resend attempts (5 minutes at 100ms interval)
	ResendMaxAttempt = 3000

	// OutgoingMessageBufferCapacity is the maximum number of messages in the outgoing buffer
	OutgoingMessageBufferCapacity = 10000

	// RTTConstant is the smoothing factor for RTT calculation (1/8)
	RTTConstant = 0.125

	// RTTVConstant is the smoothing factor for RTTVAR calculation (1/4)
	RTTVConstant = 0.25

	// ClockGranularity is the minimum granularity for timeout calculation
	ClockGranularity = 10 * time.Millisecond

	// MaxTransmissionTimeout is the maximum allowed retransmission timeout
	MaxTransmissionTimeout = 1 * time.Second
)
