//go:build integration

package agent

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/ec2"
	"github.com/wadahiro/awsocks/internal/protocol"
	"github.com/wadahiro/awsocks/internal/testutil"
)

// messageRouter reads messages from agent and routes them to appropriate channels
type messageRouter struct {
	t          *testing.T
	conn       net.Conn
	acks       chan *protocol.Message
	data       chan *protocol.Message
	closes     chan *protocol.Message
	errors     chan *protocol.Message
	logs       chan *protocol.Message
	done       chan struct{}
	stopOnce   sync.Once
}

func newMessageRouter(t *testing.T, conn net.Conn) *messageRouter {
	return &messageRouter{
		t:      t,
		conn:   conn,
		acks:   make(chan *protocol.Message, 10),
		data:   make(chan *protocol.Message, 100),
		closes: make(chan *protocol.Message, 10),
		errors: make(chan *protocol.Message, 10),
		logs:   make(chan *protocol.Message, 100),
		done:   make(chan struct{}),
	}
}

func (r *messageRouter) start() {
	go func() {
		for {
			select {
			case <-r.done:
				return
			default:
			}

			msg, err := protocol.ReadMessage(r.conn)
			if err != nil {
				return
			}

			switch msg.Type {
			case protocol.MsgConnectAck:
				r.acks <- msg
			case protocol.MsgData:
				r.data <- msg
			case protocol.MsgClose:
				r.closes <- msg
			case protocol.MsgError:
				r.errors <- msg
			case protocol.MsgLog:
				// Log messages from agent - just log them
				r.t.Logf("[Agent] %s", string(msg.Payload))
			default:
				r.t.Logf("Unknown message type: %d", msg.Type)
			}
		}
	}()
}

func (r *messageRouter) stop() {
	r.stopOnce.Do(func() {
		close(r.done)
	})
}

// TestIntegration_AgentWithMuxSSH simulates VM mode without macOS Virtualization
// Uses net.Pipe() to simulate vsock communication
func TestIntegration_AgentWithMuxSSH(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Load test configuration from environment
	testCfg := testutil.LoadTestConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(testCfg.Region),
		config.WithSharedConfigProfile(testCfg.Profile),
	)
	require.NoError(t, err)

	// Resolve instance ID
	ec2Client := ec2.NewClient(cfg)
	resolver := ec2.NewResolver(ec2Client)
	instances, err := resolver.ResolveByName(ctx, testCfg.InstanceName)
	require.NoError(t, err)
	require.NotEmpty(t, instances)
	instanceID := instances[0].ID
	t.Logf("Using instance: %s (%s)", testCfg.InstanceName, instanceID)

	// Read SSH key
	sshKeyContent, err := os.ReadFile(testCfg.SSHKeyPath)
	require.NoError(t, err)

	// Create pipe to simulate vsock
	hostConn, agentConn := net.Pipe()
	defer hostConn.Close()
	defer agentConn.Close()

	// Create Agent
	agent := NewWithoutBackend(agentConn)

	// Start Agent in background
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agent.Run()
	}()

	// Start message router to handle async messages from agent (especially logs)
	router := newMessageRouter(t, hostConn)
	router.start()
	defer router.stop()

	// Send backend config
	backendCfg := &protocol.BackendConfigPayload{
		Type:          "muxssh",
		InstanceID:    instanceID,
		Region:        testCfg.Region,
		SSHUser:       testCfg.SSHUser,
		SSHKeyContent: sshKeyContent,
	}

	err = protocol.WriteMessage(hostConn, &protocol.Message{
		Type:    protocol.MsgBackendConfig,
		Payload: mustMarshalBackendConfig(backendCfg),
	})
	require.NoError(t, err)
	t.Log("Backend config sent")

	// Send credentials
	creds, err := cfg.Credentials.Retrieve(ctx)
	require.NoError(t, err)

	credPayload := protocol.CredentialPayload{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Expiration:      creds.Expires,
	}
	credMsg := protocol.NewCredentialUpdateMessage(credPayload)
	err = protocol.WriteMessage(hostConn, credMsg)
	require.NoError(t, err)
	t.Log("Credentials sent")

	// Wait for backend to initialize
	t.Log("Waiting for backend to initialize...")
	time.Sleep(10 * time.Second)

	// Send connect request
	connID := uint32(1)
	err = protocol.WriteMessage(hostConn, &protocol.Message{
		Type:    protocol.MsgConnect,
		ConnID:  connID,
		Payload: []byte("tcp:httpbin.org:80"),
	})
	require.NoError(t, err)
	t.Log("Connect request sent")

	// Wait for connect ack
	select {
	case msg := <-router.acks:
		require.Equal(t, protocol.MsgConnectAck, msg.Type)
		t.Log("Connection established via Agent")
	case msg := <-router.errors:
		t.Fatalf("Connection failed: %s", string(msg.Payload))
	case <-time.After(2 * time.Minute):
		t.Fatal("Timeout waiting for connect ack")
	}

	// Send HTTP request
	httpReq := []byte("GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n")
	err = protocol.WriteMessage(hostConn, &protocol.Message{
		Type:    protocol.MsgData,
		ConnID:  connID,
		Payload: httpReq,
	})
	require.NoError(t, err)
	t.Log("HTTP request sent")

	// Read response
	select {
	case msg := <-router.data:
		require.Equal(t, protocol.MsgData, msg.Type)
		require.Greater(t, len(msg.Payload), 0)
		t.Logf("Received %d bytes: %s", len(msg.Payload), string(msg.Payload[:min(len(msg.Payload), 200)]))
	case <-time.After(30 * time.Second):
		t.Fatal("Timeout waiting for response data")
	}

	// Shutdown agent
	err = protocol.WriteMessage(hostConn, &protocol.Message{Type: protocol.MsgShutdown})
	require.NoError(t, err)

	// Stop router first to avoid competing for reads
	router.stop()

	// Close host connection to unblock agent's ReadMessage
	hostConn.Close()

	select {
	case err := <-agentDone:
		// Agent returns nil on EOF (normal shutdown)
		if err != nil {
			t.Logf("Agent returned error (may be expected): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not shut down")
	}
}

func mustMarshalBackendConfig(cfg *protocol.BackendConfigPayload) []byte {
	msg := protocol.NewBackendConfigMessage(cfg)
	return msg.Payload
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
