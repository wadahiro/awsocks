package ssm

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"golang.org/x/crypto/ssh"
)

// dialViaUpstreamProxy connects to the target address through an upstream HTTP CONNECT proxy.
// The proxy connection itself is established via SSH direct-tcpip channel.
func dialViaUpstreamProxy(client *ssh.Client, proxyURL string, network, address string) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream proxy URL: %w", err)
	}

	switch u.Scheme {
	case "http":
		return dialViaHTTPProxy(client, u, network, address)
	default:
		return nil, fmt.Errorf("unsupported upstream proxy scheme: %s (supported: http)", u.Scheme)
	}
}

// dialViaHTTPProxy connects to the target via HTTP CONNECT through the SSH tunnel.
func dialViaHTTPProxy(client *ssh.Client, proxyURL *url.URL, network, address string) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
		// No port specified, default to 8080 for HTTP proxy
		proxyAddr = net.JoinHostPort(proxyAddr, "8080")
	}

	// Connect to the upstream proxy via SSH direct-tcpip
	proxyConn, err := client.Dial(network, proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to upstream proxy %s: %w", proxyAddr, err)
	}

	// Send HTTP CONNECT request
	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: address},
		Host:   address,
		Header: make(http.Header),
	}

	// Add proxy authentication if provided
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		connectReq.SetBasicAuth(proxyURL.User.Username(), password)
	}

	if err := connectReq.Write(proxyConn); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("failed to send CONNECT request to upstream proxy: %w", err)
	}

	// Read response
	br := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("failed to read CONNECT response from upstream proxy: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		proxyConn.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT failed: %s", resp.Status)
	}

	// If there's buffered data in the reader, wrap the connection
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: proxyConn, reader: br}, nil
	}

	return proxyConn, nil
}

// bufferedConn wraps a net.Conn with a buffered reader for any data
// that was read ahead during the HTTP response parsing.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}
