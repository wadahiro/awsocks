package fakessm_test

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wadahiro/awsocks/internal/testutil/fakessm"
	"golang.org/x/crypto/ssh"
)

func TestSSHServer_Basic(t *testing.T) {
	// Create and start SSH server
	server, err := fakessm.NewSSHServer()
	require.NoError(t, err)

	addr, err := server.Start()
	require.NoError(t, err)
	defer server.Close()

	t.Logf("SSH server started at %s", addr)
	t.Logf("Private key length: %d bytes", len(server.PrivateKey()))

	// Parse the private key
	signer, err := ssh.ParsePrivateKey(server.PrivateKey())
	require.NoError(t, err, "failed to parse private key")

	// Connect to SSH server
	config := &ssh.ClientConfig{
		User: "testuser",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err, "failed to connect to SSH server")
	defer conn.Close()

	// SSH handshake
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	require.NoError(t, err, "SSH handshake failed")
	defer sshConn.Close()

	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	t.Log("SSH handshake successful")

	// Test direct-tcpip (port forwarding)
	targetConn, err := client.Dial("tcp", "example.com:80")
	require.NoError(t, err, "failed to dial through SSH")
	defer targetConn.Close()

	t.Log("direct-tcpip channel opened")

	// Test data echo
	testData := []byte("Hello SSH!")
	_, err = targetConn.Write(testData)
	require.NoError(t, err)

	buf := make([]byte, 1024)
	targetConn.SetDeadline(time.Now().Add(2 * time.Second))
	n, err := targetConn.Read(buf)
	require.NoError(t, err)

	assert.Equal(t, testData, buf[:n])
	t.Log("Data echo successful")
}
