package ssm_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/backend/ssm"
	"github.com/wadahiro/awsocks/internal/testutil/fakessm"
)

// TestBackend_DialWithFakeSSM tests the complete MuxSSH backend flow:
// Client -> MuxSSH Backend -> DataChannel -> Fake SSM -> Fake SSH Server
func TestBackend_DialWithFakeSSM(t *testing.T) {
	// 1. Start Fake SSH Server
	sshServer, err := fakessm.NewSSHServer()
	require.NoError(t, err, "failed to create SSH server")

	sshAddr, err := sshServer.Start()
	require.NoError(t, err, "failed to start SSH server")
	defer sshServer.Close()

	t.Logf("SSH server started at %s", sshAddr)

	// 2. Start Fake SSM Server with SSH forwarding
	ssmServer := fakessm.NewServer(&fakessm.ServerOptions{
		AgentVersion:  "3.1.0.0",
		SSHServerAddr: sshAddr,
	})
	ssmServer.Start()
	defer ssmServer.Close()

	t.Logf("SSM server started at %s", ssmServer.URL())

	// 3. Create MuxSSH Backend with Fake SSM as the SSMClient
	backend := ssm.New(&ssm.Config{
		InstanceID: "i-test-instance",
		Region:     "us-east-1",
		SSHUser:    "testuser",
	}, ssmServer)

	// 4. Set SSH key (use the test key from SSH server)
	err = backend.SetSSHKeyContent(sshServer.PrivateKey(), "")
	require.NoError(t, err, "failed to set SSH key")

	// 5. Start backend
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = backend.Start(ctx)
	require.NoError(t, err, "failed to start backend")
	defer backend.Close()

	// 6. Trigger connection by sending credentials
	err = backend.OnCredentialUpdate(aws.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "testsecret",
		SessionToken:    "testtoken",
	})
	require.NoError(t, err, "failed to update credentials")

	// 7. Wait for backend to become active
	require.Eventually(t, func() bool {
		return backend.State() == ssm.StateActive
	}, 15*time.Second, 100*time.Millisecond, "backend did not become active")

	t.Log("Backend is now active")

	// 8. Dial through the SSH tunnel
	conn, err := backend.Dial(ctx, "tcp", "example.com:80")
	require.NoError(t, err, "failed to dial through backend")
	defer conn.Close()

	t.Log("Successfully dialed through SSH tunnel")

	// 9. Test data transfer through the tunnel
	testData := []byte("Hello through SSH tunnel!")
	n, err := conn.Write(testData)
	require.NoError(t, err, "failed to write data")
	assert.Equal(t, len(testData), n, "wrote unexpected number of bytes")

	// 10. Read response (SSH server echoes data back)
	buf := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = conn.Read(buf)
	require.NoError(t, err, "failed to read data")
	assert.Equal(t, testData, buf[:n], "received data does not match sent data")

	t.Log("Data transfer through SSH tunnel successful")
}

// TestBackend_MultipleDials tests multiple Dial calls on the same backend
func TestBackend_MultipleDials(t *testing.T) {
	// Setup servers
	sshServer, err := fakessm.NewSSHServer()
	require.NoError(t, err)
	sshAddr, err := sshServer.Start()
	require.NoError(t, err)
	defer sshServer.Close()

	ssmServer := fakessm.NewServer(&fakessm.ServerOptions{
		AgentVersion:  "3.1.0.0",
		SSHServerAddr: sshAddr,
	})
	ssmServer.Start()
	defer ssmServer.Close()

	// Create and start backend
	backend := ssm.New(&ssm.Config{
		InstanceID: "i-test-instance",
		Region:     "us-east-1",
		SSHUser:    "testuser",
	}, ssmServer)

	err = backend.SetSSHKeyContent(sshServer.PrivateKey(), "")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = backend.Start(ctx)
	require.NoError(t, err)
	defer backend.Close()

	err = backend.OnCredentialUpdate(aws.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "testsecret",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return backend.State() == ssm.StateActive
	}, 15*time.Second, 100*time.Millisecond)

	// Test multiple dials
	const numDials = 5
	for i := 0; i < numDials; i++ {
		conn, err := backend.Dial(ctx, "tcp", "target.example.com:443")
		require.NoError(t, err, "dial %d failed", i)

		// Send and receive data
		testData := []byte("test data")
		_, err = conn.Write(testData)
		require.NoError(t, err)

		buf := make([]byte, 1024)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, testData, buf[:n])

		conn.Close()
	}

	t.Logf("Successfully completed %d dials", numDials)
}

// TestBackend_ParallelDials tests parallel Dial calls
func TestBackend_ParallelDials(t *testing.T) {
	// Setup servers
	sshServer, err := fakessm.NewSSHServer()
	require.NoError(t, err)
	sshAddr, err := sshServer.Start()
	require.NoError(t, err)
	defer sshServer.Close()

	ssmServer := fakessm.NewServer(&fakessm.ServerOptions{
		AgentVersion:  "3.1.0.0",
		SSHServerAddr: sshAddr,
	})
	ssmServer.Start()
	defer ssmServer.Close()

	// Create and start backend
	backend := ssm.New(&ssm.Config{
		InstanceID: "i-test-instance",
		Region:     "us-east-1",
		SSHUser:    "testuser",
	}, ssmServer)

	err = backend.SetSSHKeyContent(sshServer.PrivateKey(), "")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err = backend.Start(ctx)
	require.NoError(t, err)
	defer backend.Close()

	err = backend.OnCredentialUpdate(aws.Credentials{
		AccessKeyID:     "AKIATEST",
		SecretAccessKey: "testsecret",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return backend.State() == ssm.StateActive
	}, 15*time.Second, 100*time.Millisecond)

	// Parallel dials
	const numParallel = 10
	errors := make(chan error, numParallel)
	done := make(chan struct{}, numParallel)

	for i := 0; i < numParallel; i++ {
		go func(id int) {
			conn, err := backend.Dial(ctx, "tcp", "parallel.example.com:8080")
			if err != nil {
				errors <- err
				return
			}
			defer conn.Close()

			// Send and receive
			testData := []byte("parallel test")
			if _, err := conn.Write(testData); err != nil {
				errors <- err
				return
			}

			buf := make([]byte, 1024)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Read(buf); err != nil {
				errors <- err
				return
			}

			done <- struct{}{}
		}(i)
	}

	// Wait for all to complete
	successCount := 0
	for i := 0; i < numParallel; i++ {
		select {
		case err := <-errors:
			t.Errorf("parallel dial failed: %v", err)
		case <-done:
			successCount++
		case <-time.After(30 * time.Second):
			t.Fatal("timeout waiting for parallel dials")
		}
	}

	assert.Equal(t, numParallel, successCount, "not all parallel dials succeeded")
	t.Logf("Successfully completed %d parallel dials", successCount)
}
