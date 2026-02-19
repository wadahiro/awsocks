package awsapi

import (
	"github.com/wadahiro/awsocks/internal/mux"
)

// NewVsockDialer creates a DialContextFunc that dials via the shared AgentMux.
// This allows HTTP/WebSocket connections to be routed through the VM's NAT network.
func NewVsockDialer(agentMux *mux.AgentMux) DialContextFunc {
	return agentMux.Dial
}
