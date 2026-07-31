package datachannel

import (
	"os"
	"time"
)

// dcProfileEnabled turns on receive-loop profiling when AWSOCKS_DC_PROFILE=1.
// Evaluated once at process start; profiling is a diagnostic aid for
// investigating SSM DataChannel throughput, off by default.
var dcProfileEnabled = os.Getenv("AWSOCKS_DC_PROFILE") == "1"

// recvProfiler measures where time goes in the WebSocket receive loop:
// blocking on ReadMessage (waiting for the SSM server to send more data)
// vs. onMessage processing (ACK send + deliver to the SSH pipe). It emits a
// one-line summary roughly once per second. When disabled, every method is a
// cheap no-op so the hot path pays nothing.
//
// Why this exists: single-connection SSM throughput tops out around ~870KB/s,
// and profiling confirmed ~97% of the receive loop is spent blocked in
// ReadMessage (server-side/bandwidth bound), not in client-side ACK/deliver.
// Keep it so that hypothesis can be re-checked cheaply if throughput is ever
// questioned again.
type recvProfiler struct {
	enabled     bool
	windowStart time.Time
	readWait    time.Duration // time blocked in ReadMessage this window
	onMsg       time.Duration // time in onMessage this window
	bytes       int64         // payload bytes received this window
	msgs        int64         // messages received this window
}

// newRecvProfiler returns a profiler that is active only when
// AWSOCKS_DC_PROFILE=1. A disabled profiler's methods are no-ops.
func newRecvProfiler() *recvProfiler {
	p := &recvProfiler{enabled: dcProfileEnabled}
	if p.enabled {
		p.windowStart = time.Now()
	}
	return p
}

// beforeRead returns a timestamp to pass to afterRead, or the zero time when
// disabled.
func (p *recvProfiler) beforeRead() time.Time {
	if !p.enabled {
		return time.Time{}
	}
	return time.Now()
}

// afterRead records a completed ReadMessage: how long it blocked and how many
// payload bytes it returned.
func (p *recvProfiler) afterRead(start time.Time, n int) {
	if !p.enabled {
		return
	}
	p.readWait += time.Since(start)
	p.bytes += int64(n)
	p.msgs++
}

// beforeOnMessage returns a timestamp to pass to afterOnMessage, or the zero
// time when disabled.
func (p *recvProfiler) beforeOnMessage() time.Time {
	if !p.enabled {
		return time.Time{}
	}
	return time.Now()
}

// afterOnMessage records onMessage duration and flushes a summary line once
// the current window reaches ~1s.
func (p *recvProfiler) afterOnMessage(start time.Time) {
	if !p.enabled {
		return
	}
	p.onMsg += time.Since(start)

	elapsed := time.Since(p.windowStart)
	if elapsed < time.Second {
		return
	}

	var avgMsg int64
	if p.msgs > 0 {
		avgMsg = p.bytes / p.msgs
	}
	wsLogger.Info("DC recv profile",
		"window_ms", elapsed.Milliseconds(),
		"msgs", p.msgs,
		"bytes", p.bytes,
		"avg_msg_bytes", avgMsg,
		"read_wait_ms", p.readWait.Milliseconds(),
		"onmsg_ms", p.onMsg.Milliseconds(),
		"throughput_KBps", float64(p.bytes)/1024.0/elapsed.Seconds(),
	)

	p.readWait, p.onMsg, p.bytes, p.msgs = 0, 0, 0, 0
	p.windowStart = time.Now()
}
