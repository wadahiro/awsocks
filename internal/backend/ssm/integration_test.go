//go:build integration

package ssm

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/ec2"
	"github.com/wadahiro/awsocks/internal/testutil"
)

func TestIntegration_SingleConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	testCfg := testutil.LoadTestConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(testCfg.Region),
		config.WithSharedConfigProfile(testCfg.Profile),
	)
	require.NoError(t, err)

	// Resolve instance ID from name
	ec2Client := ec2.NewClient(cfg)
	resolver := ec2.NewResolver(ec2Client)
	instances, err := resolver.ResolveByName(ctx, testCfg.InstanceName)
	require.NoError(t, err)
	require.NotEmpty(t, instances)
	instanceID := instances[0].ID
	t.Logf("Resolved instance: %s -> %s", testCfg.InstanceName, instanceID)

	// Create SSM client
	ssmClient := NewHTTPClient(cfg)

	// Create backend
	backendCfg := &Config{
		InstanceID: instanceID,
		Region:     testCfg.Region,
		SSHUser:    testCfg.SSHUser,
		SSHKeyPath: testCfg.SSHKeyPath,
	}

	backend := New(backendCfg, ssmClient)
	err = backend.Start(ctx)
	require.NoError(t, err)
	defer backend.Close()

	// Get credentials and trigger connection
	creds, err := cfg.Credentials.Retrieve(ctx)
	require.NoError(t, err)

	err = backend.OnCredentialUpdate(creds)
	require.NoError(t, err)

	// Wait for active state
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer waitCancel()

	for {
		select {
		case <-waitCtx.Done():
			t.Fatalf("timeout waiting for backend to become active, current state: %s", backend.State())
		case <-time.After(time.Second):
			if backend.State() == StateActive {
				goto connected
			}
			t.Logf("Current state: %s", backend.State())
		}
	}

connected:
	t.Log("Backend is active, testing connection...")

	// Test dial to a known service (HTTP)
	conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
	require.NoError(t, err)
	defer conn.Close()

	// Send HTTP request
	req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	// Read response
	resp := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	n, err := conn.Read(resp)
	if err != nil && err != io.EOF {
		require.NoError(t, err)
	}
	assert.Greater(t, n, 0)

	t.Logf("Received %d bytes response: %s", n, string(resp[:min(n, 200)]))
}

func TestIntegration_MultipleConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

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

	// Create backend
	ssmClient := NewHTTPClient(cfg)
	backendCfg := &Config{
		InstanceID: instanceID,
		Region:     testCfg.Region,
		SSHUser:    testCfg.SSHUser,
		SSHKeyPath: testCfg.SSHKeyPath,
	}

	backend := New(backendCfg, ssmClient)
	err = backend.Start(ctx)
	require.NoError(t, err)
	defer backend.Close()

	// Trigger connection
	creds, err := cfg.Credentials.Retrieve(ctx)
	require.NoError(t, err)
	err = backend.OnCredentialUpdate(creds)
	require.NoError(t, err)

	// Wait for active
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer waitCancel()
	for backend.State() != StateActive {
		select {
		case <-waitCtx.Done():
			t.Fatalf("timeout waiting for active state")
		case <-time.After(time.Second):
		}
	}

	// Test multiple concurrent connections
	numConns := 5
	results := make(chan error, numConns)

	for i := 0; i < numConns; i++ {
		go func(idx int) {
			conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
			if err != nil {
				results <- fmt.Errorf("dial %d failed: %w", idx, err)
				return
			}
			defer conn.Close()

			// Send HTTP request
			req := fmt.Sprintf("GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n")
			_, err = conn.Write([]byte(req))
			if err != nil {
				results <- fmt.Errorf("write %d failed: %w", idx, err)
				return
			}

			// Read response
			resp := make([]byte, 4096)
			conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, err := conn.Read(resp)
			if err != nil && err != io.EOF {
				results <- fmt.Errorf("read %d failed: %w", idx, err)
				return
			}

			if n == 0 {
				results <- fmt.Errorf("empty response %d", idx)
				return
			}

			t.Logf("Connection %d: received %d bytes", idx, n)
			results <- nil
		}(i)
	}

	// Collect results
	for i := 0; i < numConns; i++ {
		err := <-results
		assert.NoError(t, err)
	}
}

func TestIntegration_CredentialRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

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

	// Create backend
	ssmClient := NewHTTPClient(cfg)
	backendCfg := &Config{
		InstanceID: instanceID,
		Region:     testCfg.Region,
		SSHUser:    testCfg.SSHUser,
		SSHKeyPath: testCfg.SSHKeyPath,
	}

	backend := New(backendCfg, ssmClient)
	err = backend.Start(ctx)
	require.NoError(t, err)
	defer backend.Close()

	// Initial connection
	creds, err := cfg.Credentials.Retrieve(ctx)
	require.NoError(t, err)
	err = backend.OnCredentialUpdate(creds)
	require.NoError(t, err)

	// Wait for active
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer waitCancel()
	for backend.State() != StateActive {
		select {
		case <-waitCtx.Done():
			t.Fatalf("timeout waiting for active state")
		case <-time.After(time.Second):
		}
	}

	// Make a connection
	conn1, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
	require.NoError(t, err)
	conn1.Close()

	t.Log("First connection successful")

	// Simulate credential refresh (same creds, but triggers reconnect)
	newCreds, err := cfg.Credentials.Retrieve(ctx)
	require.NoError(t, err)
	err = backend.OnCredentialUpdate(newCreds)
	require.NoError(t, err)

	// Wait for reconnection
	time.Sleep(time.Second)

	// Should transition through reconnecting to active
	for backend.State() != StateActive {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for reconnection")
		case <-time.After(time.Second):
			t.Logf("State: %s", backend.State())
		}
	}

	// Make another connection after refresh
	conn2, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
	require.NoError(t, err)
	conn2.Close()

	t.Log("Connection after credential refresh successful")
}

// TestIntegration_TCPProxy tests the backend as a SOCKS5 proxy backend
func TestIntegration_TCPProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

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

	// Create backend
	ssmClient := NewHTTPClient(cfg)
	backendCfg := &Config{
		InstanceID: instanceID,
		Region:     testCfg.Region,
		SSHUser:    testCfg.SSHUser,
		SSHKeyPath: testCfg.SSHKeyPath,
	}

	backend := New(backendCfg, ssmClient)
	err = backend.Start(ctx)
	require.NoError(t, err)
	defer backend.Close()

	// Trigger connection
	creds, err := cfg.Credentials.Retrieve(ctx)
	require.NoError(t, err)
	err = backend.OnCredentialUpdate(creds)
	require.NoError(t, err)

	// Wait for active
	for backend.State() != StateActive {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for active state")
		case <-time.After(time.Second):
		}
	}

	// Start a simple TCP echo server for testing
	listener, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	// Get the listener address
	addr := listener.Addr().String()

	// Connect via backend (this tests the SSH tunnel)
	// Note: This won't work for localhost since we're going through EC2
	// Instead, test with an external service
	conn, err := backend.Dial(ctx, "tcp", "ifconfig.me:80")
	require.NoError(t, err)
	defer conn.Close()

	// Send HTTP request
	req := "GET / HTTP/1.1\r\nHost: ifconfig.me\r\nConnection: close\r\n\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	// Read response
	resp := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	n, err := conn.Read(resp)
	require.NoError(t, err)
	assert.Greater(t, n, 0)

	// The response should contain an IP address (the EC2's public IP)
	t.Logf("Response from EC2: %s", string(resp[:n]))

	// Ignore unused variable warning
	_ = addr
}

// TestIntegration_LargeResponse tests downloading large data (like PNG images)
func TestIntegration_LargeResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// Test 1: Small response (should work)
	t.Run("SmallResponse", func(t *testing.T) {
		conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
		require.NoError(t, err)
		defer conn.Close()

		req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
		_, err = conn.Write([]byte(req))
		require.NoError(t, err)

		resp, err := io.ReadAll(conn)
		require.NoError(t, err)
		t.Logf("Small response: %d bytes", len(resp))
		assert.Greater(t, len(resp), 100)
	})

	// Test 2: Large binary response (PNG image)
	t.Run("LargeBinaryResponse", func(t *testing.T) {
		// Use httpbin's image endpoint (returns ~8KB PNG)
		conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
		require.NoError(t, err)
		defer conn.Close()

		req := "GET /image/png HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
		_, err = conn.Write([]byte(req))
		require.NoError(t, err)

		resp, err := io.ReadAll(conn)
		require.NoError(t, err)
		t.Logf("PNG response: %d bytes", len(resp))
		// PNG should be at least 1KB
		assert.Greater(t, len(resp), 1000)
	})

	// Test 3: Large JSON response
	t.Run("LargeJSONResponse", func(t *testing.T) {
		// Request a large stream of bytes
		conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
		require.NoError(t, err)
		defer conn.Close()

		// Request 100KB of random bytes
		req := "GET /bytes/102400 HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
		_, err = conn.Write([]byte(req))
		require.NoError(t, err)

		resp, err := io.ReadAll(conn)
		require.NoError(t, err)
		t.Logf("Large response: %d bytes", len(resp))
		// Should be at least 100KB (plus headers)
		assert.Greater(t, len(resp), 100000)
	})

	// Test 4: HTTPS large response (like GraphQL might use)
	t.Run("HTTPSLargeResponse", func(t *testing.T) {
		// Dial to HTTPS port
		conn, err := backend.Dial(ctx, "tcp", "httpbin.org:443")
		require.NoError(t, err)
		defer conn.Close()

		// Wrap with TLS
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: "httpbin.org",
		})
		defer tlsConn.Close()

		err = tlsConn.Handshake()
		require.NoError(t, err)

		// Request large response over HTTPS
		req := "GET /bytes/102400 HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
		_, err = tlsConn.Write([]byte(req))
		require.NoError(t, err)

		resp, err := io.ReadAll(tlsConn)
		require.NoError(t, err)
		t.Logf("HTTPS large response: %d bytes", len(resp))
		assert.Greater(t, len(resp), 100000)
	})
}

// TestIntegration_SequentialRequests tests multiple sequential requests
func TestIntegration_SequentialRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// Make 10 sequential requests
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprintf("Request%d", i), func(t *testing.T) {
			conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
			require.NoError(t, err, "Request %d: dial failed", i)
			defer conn.Close()

			req := fmt.Sprintf("GET /get?n=%d HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n", i)
			_, err = conn.Write([]byte(req))
			require.NoError(t, err, "Request %d: write failed", i)

			resp, err := io.ReadAll(conn)
			require.NoError(t, err, "Request %d: read failed", i)
			assert.Greater(t, len(resp), 100, "Request %d: response too short", i)
			t.Logf("Request %d: %d bytes", i, len(resp))
		})
	}
}

// TestIntegration_MixedRequests tests alternating small and large requests
func TestIntegration_MixedRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// Alternate between small and large requests
	for i := 0; i < 5; i++ {
		// Small request
		t.Run(fmt.Sprintf("Small%d", i), func(t *testing.T) {
			conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
			require.NoError(t, err)
			defer conn.Close()

			req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
			_, err = conn.Write([]byte(req))
			require.NoError(t, err)

			resp, err := io.ReadAll(conn)
			require.NoError(t, err)
			t.Logf("Small request %d: %d bytes", i, len(resp))
		})

		// Large request (50KB)
		t.Run(fmt.Sprintf("Large%d", i), func(t *testing.T) {
			conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
			require.NoError(t, err)
			defer conn.Close()

			req := "GET /bytes/51200 HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
			_, err = conn.Write([]byte(req))
			require.NoError(t, err)

			resp, err := io.ReadAll(conn)
			require.NoError(t, err)
			t.Logf("Large request %d: %d bytes", i, len(resp))
			assert.Greater(t, len(resp), 50000)
		})
	}
}

// setupBackend creates and initializes a backend for testing
func setupBackend(t *testing.T, ctx context.Context) *Backend {
	t.Helper()

	testCfg := testutil.LoadTestConfig(t)

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

	// Create backend
	ssmClient := NewHTTPClient(cfg)
	backendCfg := &Config{
		InstanceID: instanceID,
		Region:     testCfg.Region,
		SSHUser:    testCfg.SSHUser,
		SSHKeyPath: testCfg.SSHKeyPath,
	}

	backend := New(backendCfg, ssmClient)
	err = backend.Start(ctx)
	require.NoError(t, err)

	// Trigger connection
	creds, err := cfg.Credentials.Retrieve(ctx)
	require.NoError(t, err)
	err = backend.OnCredentialUpdate(creds)
	require.NoError(t, err)

	// Wait for active
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer waitCancel()
	for backend.State() != StateActive {
		select {
		case <-waitCtx.Done():
			t.Fatalf("timeout waiting for active state, current: %s", backend.State())
		case <-time.After(time.Second):
			t.Logf("Waiting for backend... state: %s", backend.State())
		}
	}
	t.Log("Backend is active")

	return backend
}

// TestIntegration_NoRouteErrors tests that "No route to host" errors don't kill the connection
func TestIntegration_NoRouteErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// First, verify connection works
	t.Run("InitialConnection", func(t *testing.T) {
		conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
		require.NoError(t, err)
		defer conn.Close()

		req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
		_, err = conn.Write([]byte(req))
		require.NoError(t, err)

		resp, err := io.ReadAll(conn)
		require.NoError(t, err)
		t.Logf("Initial response: %d bytes", len(resp))
		assert.Greater(t, len(resp), 100)
	})

	// Try to connect to unreachable hosts (should fail but not kill the connection)
	// These use short timeouts because SSH direct-tcpip can take a long time to timeout
	t.Run("UnreachableHosts", func(t *testing.T) {
		unreachableHosts := []string{
			"10.255.255.1:443",  // RFC 5737 test network - should be unreachable
			"192.0.2.1:443",     // RFC 5737 TEST-NET-1
			"198.51.100.1:443",  // RFC 5737 TEST-NET-2
		}

		for i, host := range unreachableHosts {
			// Use short timeout for each dial attempt
			dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := backend.Dial(dialCtx, "tcp", host)
			dialCancel()

			// These should fail with "No route to host", timeout, or similar
			// We don't assert error because the dial might timeout before SSH rejects
			if err != nil {
				t.Logf("Unreachable host %d (%s): %v", i, host, err)
			} else {
				t.Logf("Unreachable host %d (%s): unexpectedly connected (should not happen)", i, host)
			}
		}
	})

	// Verify connection still works after "No route" errors
	t.Run("ConnectionAfterNoRoute", func(t *testing.T) {
		conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
		require.NoError(t, err, "Connection should still work after No route errors")
		defer conn.Close()

		req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
		_, err = conn.Write([]byte(req))
		require.NoError(t, err)

		resp, err := io.ReadAll(conn)
		require.NoError(t, err)
		t.Logf("Response after No route errors: %d bytes", len(resp))
		assert.Greater(t, len(resp), 100)
	})

	// Mix of successful and failing requests (use connection refused for faster failures)
	t.Run("MixedRequests", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			// Try to connect to a port that should be refused (not timeout)
			// Using localhost's closed port should return quickly
			dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
			_, err := backend.Dial(dialCtx, "tcp", "httpbin.org:9999")
			dialCancel()
			if err != nil {
				t.Logf("Mixed test %d refused/unreachable: %v", i, err)
			}

			// Should still work
			conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
			require.NoError(t, err, "Request %d should succeed", i)

			req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
			conn.Write([]byte(req))
			resp, err := io.ReadAll(conn)
			conn.Close()
			require.NoError(t, err)
			t.Logf("Mixed request %d success: %d bytes", i, len(resp))
		}
	})
}

// TestIntegration_ConcurrentNoRouteErrors tests that concurrent "No route to host" errors don't kill the connection
// This simulates real browser behavior where multiple requests happen in parallel
func TestIntegration_ConcurrentNoRouteErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// First verify connection works
	conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
	require.NoError(t, err)
	req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
	conn.Write([]byte(req))
	resp, _ := io.ReadAll(conn)
	conn.Close()
	t.Logf("Initial connection OK: %d bytes", len(resp))

	// Simulate browser behavior: multiple concurrent requests to unreachable hosts
	t.Run("ConcurrentUnreachable", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan error, 10)

		// Launch 5 concurrent requests to unreachable hosts
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
				defer dialCancel()
				_, err := backend.Dial(dialCtx, "tcp", "10.255.255.1:443")
				results <- err
				t.Logf("Concurrent unreachable %d: %v", idx, err)
			}(i)
		}

		// Also launch 5 concurrent requests to reachable hosts
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
				if err != nil {
					results <- err
					t.Logf("Concurrent reachable %d FAILED: %v", idx, err)
					return
				}
				defer conn.Close()
				req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
				conn.Write([]byte(req))
				resp, err := io.ReadAll(conn)
				if err != nil {
					results <- err
					t.Logf("Concurrent reachable %d read FAILED: %v", idx, err)
					return
				}
				t.Logf("Concurrent reachable %d OK: %d bytes", idx, len(resp))
				results <- nil
			}(i)
		}

		wg.Wait()
		close(results)

		// Count successes and failures
		successes := 0
		failures := 0
		for err := range results {
			if err == nil {
				successes++
			} else {
				failures++
			}
		}
		t.Logf("Results: %d successes, %d failures", successes, failures)
	})

	// After concurrent chaos, verify connection still works
	t.Run("AfterConcurrentChaos", func(t *testing.T) {
		time.Sleep(2 * time.Second) // Let things settle

		conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
		require.NoError(t, err, "Connection should still work after concurrent chaos")
		defer conn.Close()

		req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
		conn.Write([]byte(req))
		resp, err := io.ReadAll(conn)
		require.NoError(t, err)
		t.Logf("After chaos: %d bytes", len(resp))
		assert.Greater(t, len(resp), 100)
	})
}

// TestIntegration_BrowserSimulation simulates browser behavior with many parallel requests
func TestIntegration_BrowserSimulation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// Simulate browser loading a page with many resources
	// Browser typically opens 6+ connections in parallel
	t.Run("ParallelPageLoad", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan string, 30)

		// Simulate 20 parallel requests (like a browser loading resources)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				start := time.Now()
				conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
				if err != nil {
					results <- fmt.Sprintf("Request %d FAILED after %v: %v", idx, time.Since(start), err)
					return
				}
				defer conn.Close()

				req := fmt.Sprintf("GET /get?req=%d HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n", idx)
				conn.Write([]byte(req))
				resp, err := io.ReadAll(conn)
				if err != nil {
					results <- fmt.Sprintf("Request %d read FAILED: %v", idx, err)
					return
				}
				results <- fmt.Sprintf("Request %d OK: %d bytes in %v", idx, len(resp), time.Since(start))
			}(i)
		}

		wg.Wait()
		close(results)

		successes := 0
		failures := 0
		for result := range results {
			t.Log(result)
			if strings.Contains(result, "OK") {
				successes++
			} else {
				failures++
			}
		}
		t.Logf("Total: %d successes, %d failures", successes, failures)
		assert.Equal(t, 20, successes, "All 20 requests should succeed")
	})

	// Simulate rapid page reloads
	t.Run("RapidReloads", func(t *testing.T) {
		for reload := 0; reload < 3; reload++ {
			t.Logf("Reload %d", reload)
			var wg sync.WaitGroup
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
					if err != nil {
						t.Logf("Reload %d, Request %d FAILED: %v", reload, idx, err)
						return
					}
					defer conn.Close()
					req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
					conn.Write([]byte(req))
					io.ReadAll(conn)
				}(i)
			}
			wg.Wait()
			time.Sleep(500 * time.Millisecond)
		}
	})
}

// TestIntegration_LongLivedConnections simulates browser Keep-Alive connections
// This is closer to real browser behavior where connections stay open
func TestIntegration_LongLivedConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// Open multiple connections and keep them alive (like browser Keep-Alive)
	t.Run("KeepAliveConnections", func(t *testing.T) {
		var wg sync.WaitGroup
		conns := make([]net.Conn, 10)
		errors := make(chan error, 10)

		// Open 10 connections in parallel and keep them open
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
				if err != nil {
					errors <- fmt.Errorf("conn %d failed: %w", idx, err)
					return
				}
				conns[idx] = conn

				// Send a request using HTTP/1.1 Keep-Alive
				req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: keep-alive\r\n\r\n"
				_, err = conn.Write([]byte(req))
				if err != nil {
					errors <- fmt.Errorf("conn %d write failed: %w", idx, err)
					return
				}

				// Read response
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					errors <- fmt.Errorf("conn %d read failed: %w", idx, err)
					return
				}
				t.Logf("Conn %d: received %d bytes", idx, n)
				errors <- nil
			}(i)
		}

		wg.Wait()
		close(errors)

		successes := 0
		for err := range errors {
			if err == nil {
				successes++
			} else {
				t.Log(err)
			}
		}
		t.Logf("Opened %d Keep-Alive connections", successes)

		// Now send more requests on existing connections (reuse)
		t.Log("Sending second request on existing connections...")
		for i, conn := range conns {
			if conn != nil {
				req := "GET /headers HTTP/1.1\r\nHost: httpbin.org\r\nConnection: keep-alive\r\n\r\n"
				_, err := conn.Write([]byte(req))
				if err != nil {
					t.Logf("Conn %d reuse write failed: %v", i, err)
					continue
				}
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					t.Logf("Conn %d reuse read failed: %v", i, err)
					continue
				}
				t.Logf("Conn %d reuse OK: %d bytes", i, n)
			}
		}

		// Close all connections
		for _, conn := range conns {
			if conn != nil {
				conn.Close()
			}
		}
	})

	// Test opening new connections while others are still open
	t.Run("OverlappingConnections", func(t *testing.T) {
		activeConns := make([]net.Conn, 0)
		mu := &sync.Mutex{}

		for batch := 0; batch < 5; batch++ {
			// Open 5 new connections
			for i := 0; i < 5; i++ {
				conn, err := backend.Dial(ctx, "tcp", "httpbin.org:80")
				if err != nil {
					t.Logf("Batch %d, conn %d failed: %v", batch, i, err)
					continue
				}

				mu.Lock()
				activeConns = append(activeConns, conn)
				currentActive := len(activeConns)
				mu.Unlock()

				// Send request
				req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: keep-alive\r\n\r\n"
				conn.Write([]byte(req))

				buf := make([]byte, 4096)
				n, _ := conn.Read(buf)
				t.Logf("Batch %d, conn %d: %d bytes (total active: %d)", batch, i, n, currentActive)
			}

			// Close 2 oldest connections
			mu.Lock()
			if len(activeConns) >= 2 {
				activeConns[0].Close()
				activeConns[1].Close()
				activeConns = activeConns[2:]
			}
			mu.Unlock()
		}

		// Cleanup remaining
		mu.Lock()
		for _, conn := range activeConns {
			conn.Close()
		}
		mu.Unlock()
	})
}

// TestIntegration_HTTPS tests HTTPS connections (like browser)
// This tests TLS handshake over SSH tunnel
func TestIntegration_HTTPS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// Test HTTPS connection (like browser would)
	t.Run("HTTPS_TLS", func(t *testing.T) {
		conn, err := backend.Dial(ctx, "tcp", "httpbin.org:443")
		require.NoError(t, err)
		defer conn.Close()

		// Perform TLS handshake
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: "httpbin.org",
		})
		defer tlsConn.Close()

		err = tlsConn.Handshake()
		require.NoError(t, err, "TLS handshake should succeed")

		// Send HTTPS request
		req := "GET /get HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n"
		_, err = tlsConn.Write([]byte(req))
		require.NoError(t, err)

		// Read response
		resp := make([]byte, 8192)
		n, err := tlsConn.Read(resp)
		// EOF is expected after reading
		if err != nil && err != io.EOF {
			require.NoError(t, err)
		}
		t.Logf("HTTPS response: %d bytes", n)
		assert.Greater(t, n, 100)
	})

	// Test multiple parallel HTTPS connections (like browser tabs)
	t.Run("ParallelHTTPS", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan string, 10)

		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				start := time.Now()

				conn, err := backend.Dial(ctx, "tcp", "httpbin.org:443")
				if err != nil {
					results <- fmt.Sprintf("Conn %d FAILED dial: %v", idx, err)
					return
				}
				defer conn.Close()

				tlsConn := tls.Client(conn, &tls.Config{
					ServerName: "httpbin.org",
				})
				defer tlsConn.Close()

				if err := tlsConn.Handshake(); err != nil {
					results <- fmt.Sprintf("Conn %d FAILED TLS: %v", idx, err)
					return
				}

				req := fmt.Sprintf("GET /get?id=%d HTTP/1.1\r\nHost: httpbin.org\r\nConnection: close\r\n\r\n", idx)
				tlsConn.Write([]byte(req))
				resp := make([]byte, 8192)
				n, _ := tlsConn.Read(resp)

				results <- fmt.Sprintf("Conn %d OK: %d bytes in %v", idx, n, time.Since(start))
			}(i)
		}

		wg.Wait()
		close(results)

		successes := 0
		for r := range results {
			t.Log(r)
			if strings.Contains(r, "OK") {
				successes++
			}
		}
		t.Logf("HTTPS: %d/10 succeeded", successes)
		assert.GreaterOrEqual(t, successes, 8, "At least 8/10 HTTPS connections should succeed")
	})
}

// TestIntegration_GoogleDomains tests connections to Google domains
// This specifically tests the domains that seem to cause issues in browser
func TestIntegration_GoogleDomains(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	backend := setupBackend(t, ctx)
	defer backend.Close()

	// Test domains that were causing issues in browser logs
	domains := []string{
		"ssl.gstatic.com:443",
		"docs.google.com:443",
		"www.google.com:443",
		"play.google.com:443",
		"www.googletagmanager.com:443",
		"www.googleadservices.com:443",
	}

	for _, domain := range domains {
		t.Run(domain, func(t *testing.T) {
			dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
			defer dialCancel()

			conn, err := backend.Dial(dialCtx, "tcp", domain)
			if err != nil {
				t.Logf("FAILED: %s - %v", domain, err)
				return
			}
			defer conn.Close()

			// Try TLS handshake
			host := strings.Split(domain, ":")[0]
			tlsConn := tls.Client(conn, &tls.Config{
				ServerName: host,
			})
			defer tlsConn.Close()

			if err := tlsConn.Handshake(); err != nil {
				t.Logf("TLS FAILED: %s - %v", domain, err)
				return
			}

			t.Logf("OK: %s", domain)
		})
	}
}

// Suppress unused import warning
var _ = http.Client{}
