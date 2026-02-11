//go:build integration

package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/mode"
	"github.com/wadahiro/awsocks/internal/testutil"

	"golang.org/x/net/proxy"
)

// TestIntegration_DirectManager tests DirectManager with muxssh backend
func TestIntegration_DirectManager(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	testCfg := testutil.LoadTestConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg := &Config{
		Mode:       mode.ModeDirect,
		Name:       testCfg.InstanceName,
		Region:     testCfg.Region,
		Profile:    testCfg.Profile,
		SSHUser:    testCfg.SSHUser,
		SSHKeyPath: testCfg.SSHKeyPath,
		ListenAddr: "127.0.0.1:0", // dynamic port
	}

	mgr, err := NewDirectManager(cfg)
	require.NoError(t, err)

	// Start manager in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Start(ctx)
	}()

	// Wait for backend to become ready (up to 2 minutes)
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer waitCancel()

	var proxyAddr string
	for {
		select {
		case <-waitCtx.Done():
			t.Fatalf("timeout waiting for backend to become active")
		case err := <-errCh:
			t.Fatalf("manager failed to start: %v", err)
		case <-time.After(time.Second):
			if mgr.socks5 != nil && mgr.socks5.listener != nil {
				proxyAddr = mgr.socks5.listener.Addr().String()
				if mgr.backend != nil {
					goto ready
				}
			}
			t.Log("Waiting for backend...")
		}
	}

ready:
	defer mgr.Stop()

	// Wait additional time for SSH connection to be established
	time.Sleep(5 * time.Second)

	t.Logf("SOCKS5 proxy listening on %s", proxyAddr)

	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	require.NoError(t, err)

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	resp, err := client.Get("http://httpbin.org/get")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "httpbin.org")

	t.Logf("Response: %s", string(body[:min(len(body), 200)]))
}

// TestIntegration_DirectManager_LargeResponse tests large responses through the full SOCKS5 proxy stack
func TestIntegration_DirectManager_LargeResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	testCfg := testutil.LoadTestConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg := &Config{
		Mode:       mode.ModeDirect,
		Name:       testCfg.InstanceName,
		Region:     testCfg.Region,
		Profile:    testCfg.Profile,
		SSHUser:    testCfg.SSHUser,
		SSHKeyPath: testCfg.SSHKeyPath,
		ListenAddr: "127.0.0.1:0",
	}

	mgr, err := NewDirectManager(cfg)
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Start(ctx)
	}()

	// Wait for backend to become ready
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer waitCancel()

	var proxyAddr string
	for {
		select {
		case <-waitCtx.Done():
			t.Fatalf("timeout waiting for backend to become active")
		case err := <-errCh:
			t.Fatalf("manager failed to start: %v", err)
		case <-time.After(time.Second):
			if mgr.socks5 != nil && mgr.socks5.listener != nil {
				proxyAddr = mgr.socks5.listener.Addr().String()
				if mgr.backend != nil {
					goto ready
				}
			}
			t.Log("Waiting for backend...")
		}
	}

ready:
	defer mgr.Stop()
	time.Sleep(5 * time.Second)
	t.Logf("SOCKS5 proxy listening on %s", proxyAddr)

	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	require.NoError(t, err)

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
	}
	client := &http.Client{Transport: transport, Timeout: 60 * time.Second}

	// Test 1: Small response
	t.Run("SmallResponse", func(t *testing.T) {
		resp, err := client.Get("http://httpbin.org/get")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode)
		t.Logf("Small response: %d bytes", len(body))
	})

	// Test 2: PNG image (8KB)
	t.Run("PNGImage", func(t *testing.T) {
		resp, err := client.Get("http://httpbin.org/image/png")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode)
		assert.Greater(t, len(body), 1000)
		t.Logf("PNG response: %d bytes", len(body))
	})

	// Test 3: Large binary (100KB)
	t.Run("LargeBinary", func(t *testing.T) {
		resp, err := client.Get("http://httpbin.org/bytes/102400")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode)
		assert.Greater(t, len(body), 100000)
		t.Logf("Large binary response: %d bytes", len(body))
	})

	// Test 4: HTTPS large response
	t.Run("HTTPSLarge", func(t *testing.T) {
		resp, err := client.Get("https://httpbin.org/bytes/102400")
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode)
		assert.Greater(t, len(body), 100000)
		t.Logf("HTTPS large response: %d bytes", len(body))
	})

	// Test 5: Sequential requests
	t.Run("SequentialRequests", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			resp, err := client.Get(fmt.Sprintf("http://httpbin.org/get?n=%d", i))
			require.NoError(t, err, "request %d failed", i)

			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err, "read %d failed", i)
			assert.Equal(t, 200, resp.StatusCode)
			t.Logf("Request %d: %d bytes", i, len(body))
		}
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
