package fakessm

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// SSHServer is a fake SSH server for testing
type SSHServer struct {
	server     *ssh.Server
	listener   net.Listener
	hostKey    gossh.Signer
	privateKey []byte
	publicKey  gossh.PublicKey

	// For tracking connections
	mu          sync.RWMutex
	connections int
}

// NewSSHServer creates a new fake SSH server with auto-generated keys
func NewSSHServer() (*SSHServer, error) {
	// Generate ED25519 key pair for the server
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate host key: %w", err)
	}

	// Create SSH signer from private key
	signer, err := gossh.NewSignerFromKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	// Also generate a client key pair for testing
	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate client key: %w", err)
	}

	clientSigner, err := gossh.NewSignerFromKey(clientPriv)
	if err != nil {
		return nil, fmt.Errorf("failed to create client signer: %w", err)
	}

	// Marshal private key to PEM format for test usage
	privKeyBlock, err := gossh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}
	privKeyPEM := pem.EncodeToMemory(privKeyBlock)

	_ = pubKey // Server's public key (not needed)
	clientPublicKey, err := gossh.NewPublicKey(clientPub)
	if err != nil {
		return nil, fmt.Errorf("failed to create client public key: %w", err)
	}

	s := &SSHServer{
		hostKey:    signer,
		privateKey: privKeyPEM,
		publicKey:  clientSigner.PublicKey(),
	}

	// Create the SSH server
	s.server = &ssh.Server{
		// Accept any public key that matches our test key
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			// Accept the test public key
			return ssh.KeysEqual(key, clientPublicKey)
		},

		// Handle direct-tcpip for port forwarding
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"direct-tcpip": s.directTCPIPHandler,
		},

		// Allow local port forwarding
		LocalPortForwardingCallback: func(ctx ssh.Context, destHost string, destPort uint32) bool {
			return true
		},
	}

	s.server.AddHostKey(signer)

	return s, nil
}

// Start starts the SSH server on a random port
func (s *SSHServer) Start() (string, error) {
	var err error
	s.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("failed to listen: %w", err)
	}

	go func() {
		s.server.Serve(s.listener)
	}()

	return s.listener.Addr().String(), nil
}

// Close stops the SSH server
func (s *SSHServer) Close() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// Addr returns the server address (only valid after Start)
func (s *SSHServer) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// PrivateKey returns the test client private key in PEM format
func (s *SSHServer) PrivateKey() []byte {
	return s.privateKey
}

// PublicKey returns the test client public key
func (s *SSHServer) PublicKey() gossh.PublicKey {
	return s.publicKey
}

// directTCPIPHandler handles direct-tcpip channel requests (port forwarding)
func (s *SSHServer) directTCPIPHandler(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
	// Parse the destination from the channel request
	d := struct {
		DestAddr   string
		DestPort   uint32
		OriginAddr string
		OriginPort uint32
	}{}

	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		newChan.Reject(gossh.ConnectionFailed, "failed to parse forward data")
		return
	}

	// For testing, we'll create an echo server instead of connecting to the real destination
	// This allows us to test the tunnel without needing real network targets
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}

	go gossh.DiscardRequests(reqs)

	s.mu.Lock()
	s.connections++
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.connections--
		s.mu.Unlock()
		ch.Close()
	}()

	// Echo server: read data and echo it back
	buf := make([]byte, 32*1024)
	for {
		n, err := ch.Read(buf)
		if err != nil {
			if err != io.EOF {
				// Ignore read errors on close
			}
			return
		}

		// Echo back the data
		_, err = ch.Write(buf[:n])
		if err != nil {
			return
		}
	}
}

// ConnectionCount returns the number of active port-forward connections
func (s *SSHServer) ConnectionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connections
}
