package fakessm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	ssmbackend "github.com/wadahiro/awsocks/internal/backend/ssm"
)

// ServerOptions configures the fake SSM server behavior
type ServerOptions struct {
	// AgentVersion reported in handshake
	AgentVersion string

	// SimulateDelay adds artificial delay before handshake
	SimulateDelay time.Duration

	// SimulateErrors enables random error simulation
	SimulateErrors bool

	// SSHServerAddr is the address of the SSH server to forward data to.
	// If empty, the server operates in echo mode.
	SSHServerAddr string

	// Callbacks
	OnConnect    func(sessionID string)
	OnMessage    func(sessionID string, msg []byte)
	OnDisconnect func(sessionID string)
}

// DefaultServerOptions returns sensible defaults
func DefaultServerOptions() *ServerOptions {
	return &ServerOptions{
		AgentVersion: "3.1.0.0",
	}
}

// Server is a fake SSM server for testing
type Server struct {
	httpServer *httptest.Server
	sessions   map[string]*Session
	mu         sync.RWMutex
	opts       *ServerOptions
	upgrader   websocket.Upgrader
}

// NewServer creates a new fake SSM server
func NewServer(opts *ServerOptions) *Server {
	if opts == nil {
		opts = DefaultServerOptions()
	}

	s := &Server{
		sessions: make(map[string]*Session),
		opts:     opts,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}

	return s
}

// Start starts the fake SSM server and returns the WebSocket URL
func (s *Server) Start() string {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWebSocket)

	s.httpServer = httptest.NewServer(mux)

	// Convert http:// to ws://
	url := strings.Replace(s.httpServer.URL, "http://", "ws://", 1)
	return url
}

// Close stops the server and closes all sessions
func (s *Server) Close() {
	s.mu.Lock()
	for _, session := range s.sessions {
		session.Close()
	}
	s.sessions = make(map[string]*Session)
	s.mu.Unlock()

	if s.httpServer != nil {
		s.httpServer.Close()
	}
}

// URL returns the server URL (only valid after Start)
func (s *Server) URL() string {
	if s.httpServer == nil {
		return ""
	}
	return strings.Replace(s.httpServer.URL, "http://", "ws://", 1)
}

// handleWebSocket handles incoming WebSocket connections
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	sessionID := uuid.New().String()
	session := NewSession(sessionID, conn, s)

	s.mu.Lock()
	s.sessions[sessionID] = session
	s.mu.Unlock()

	// Run session in this goroutine (httptest handles concurrency)
	session.Run()
}

// removeSession removes a session from the server
func (s *Server) removeSession(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

// GetSession returns a session by ID
func (s *Server) GetSession(sessionID string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[sessionID]
}

// SessionCount returns the number of active sessions
func (s *Server) SessionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// Sessions returns all active sessions
func (s *Server) Sessions() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		result = append(result, session)
	}
	return result
}

// SendToAll sends data to all connected sessions
func (s *Server) SendToAll(payload []byte) error {
	s.mu.RLock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	s.mu.RUnlock()

	for _, session := range sessions {
		if err := session.SendData(payload); err != nil {
			return err
		}
	}
	return nil
}

// WaitForSessions waits until the specified number of sessions are connected
func (s *Server) WaitForSessions(count int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.SessionCount() >= count {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// WaitForHandshake waits until all sessions complete handshake
func (s *Server) WaitForHandshake(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allDone := true
		for _, session := range s.Sessions() {
			if !session.IsHandshakeComplete() {
				allDone = false
				break
			}
		}
		if allDone && s.SessionCount() > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// StartSession implements the SSMClient interface for testing.
// It returns a fake session with the WebSocket URL pointing to this server.
func (s *Server) StartSession(ctx context.Context, input *ssmbackend.StartSessionInput) (*ssmbackend.StartSessionOutput, error) {
	sessionID := uuid.New().String()
	return &ssmbackend.StartSessionOutput{
		SessionId:  sessionID,
		StreamUrl:  s.URL(),
		TokenValue: "fake-token-" + sessionID,
	}, nil
}

// TerminateSession implements the SSMClient interface for testing.
func (s *Server) TerminateSession(ctx context.Context, input *ssmbackend.TerminateSessionInput) (*ssmbackend.TerminateSessionOutput, error) {
	return &ssmbackend.TerminateSessionOutput{
		SessionId: input.SessionId,
	}, nil
}
