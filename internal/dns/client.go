// Package dns implements a minimal DNS-over-TCP client that issues queries
// through a caller-supplied dialer, typically a proxy tunnel.
//
// DNS over TCP is used rather than UDP because the tunnel transports only
// stream connections. The stdlib net.Resolver is not usable here: it calls
// SetDeadline on the connection returned by its Dial hook, which SSH channels
// do not implement, and it does not expose response TTLs.
package dns

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// maxDNSMessageSize is the largest DNS message the 2-byte length prefix allows.
const maxDNSMessageSize = 0xffff

// DialFunc opens a connection to the DNS server. It matches the signature of
// backend.Backend.Dial so a tunnel can be plugged in directly.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Answer is a single resolved address with its TTL.
type Answer struct {
	IP  net.IP
	TTL time.Duration
}

// Result holds the outcome of one query.
type Result struct {
	Answers []Answer
	Rcode   dnsmessage.RCode
	// MinTTL is the smallest TTL across all records in the response,
	// including CNAMEs, which bound how long the whole chain stays valid.
	MinTTL time.Duration
}

// Client issues DNS queries over TCP through a DialFunc.
type Client struct {
	dial    DialFunc
	servers []string
	timeout time.Duration
}

// NewClient creates a client querying the given servers in order.
// Each server must already be in host:port form.
func NewClient(dial DialFunc, servers []string, timeout time.Duration) *Client {
	return &Client{dial: dial, servers: servers, timeout: timeout}
}

// Query asks each server in order and returns the first usable response.
// A response carrying NXDOMAIN or no answers is usable: it is returned with
// the corresponding Rcode rather than as an error.
func (c *Client) Query(ctx context.Context, name string, qtype dnsmessage.Type) (*Result, error) {
	if len(c.servers) == 0 {
		return nil, fmt.Errorf("dns: no servers configured")
	}

	question, err := buildQuestion(name, qtype)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, server := range c.servers {
		res, err := c.exchange(ctx, server, question, qtype)
		if err != nil {
			lastErr = err
			continue
		}
		return res, nil
	}
	return nil, fmt.Errorf("dns: all servers failed: %w", lastErr)
}

// buildQuestion packs a query message for name/qtype.
func buildQuestion(name string, qtype dnsmessage.Type) ([]byte, error) {
	fqdn := name
	if len(fqdn) == 0 || fqdn[len(fqdn)-1] != '.' {
		fqdn += "."
	}

	dnsName, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return nil, fmt.Errorf("dns: invalid name %q: %w", name, err)
	}

	// ID is fixed at 0: each query uses a dedicated TCP connection, so there is
	// no cross-query multiplexing that would need matching IDs, and TCP already
	// protects against the off-path spoofing that random IDs guard against.
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:               0,
		RecursionDesired: true,
	})
	b.EnableCompression()

	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{
		Name:  dnsName,
		Type:  qtype,
		Class: dnsmessage.ClassINET,
	}); err != nil {
		return nil, err
	}
	return b.Finish()
}

// exchange runs one query against one server.
func (c *Client) exchange(ctx context.Context, server string, question []byte, qtype dnsmessage.Type) (*Result, error) {
	qctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	conn, err := c.dial(qctx, "tcp", server)
	if err != nil {
		return nil, fmt.Errorf("dns: dial %s: %w", server, err)
	}

	// Closing the connection is what unblocks a read stuck past the deadline:
	// tunnel connections do not support SetDeadline, so the I/O runs in a
	// goroutine and cancellation closes the connection out from under it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-qctx.Done():
			_ = conn.Close()
		case <-done:
			_ = conn.Close()
		}
	}()

	type exchangeResult struct {
		res *Result
		err error
	}
	resultCh := make(chan exchangeResult, 1)

	go func() {
		if err := writeMsg(conn, question); err != nil {
			resultCh <- exchangeResult{nil, fmt.Errorf("dns: write to %s: %w", server, err)}
			return
		}
		msg, err := readMsg(conn)
		if err != nil {
			resultCh <- exchangeResult{nil, fmt.Errorf("dns: read from %s: %w", server, err)}
			return
		}
		res, err := parseAnswers(msg, qtype)
		if err != nil {
			resultCh <- exchangeResult{nil, fmt.Errorf("dns: parse response from %s: %w", server, err)}
			return
		}
		resultCh <- exchangeResult{res, nil}
	}()

	select {
	case <-qctx.Done():
		return nil, fmt.Errorf("dns: query %s: %w", server, qctx.Err())
	case r := <-resultCh:
		return r.res, r.err
	}
}

// parseAnswers extracts A/AAAA records matching qtype from a response.
func parseAnswers(msg []byte, qtype dnsmessage.Type) (*Result, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(msg)
	if err != nil {
		return nil, err
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, err
	}

	res := &Result{Rcode: hdr.RCode, MinTTL: -1}

	for {
		ah, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return nil, err
		}

		ttl := time.Duration(ah.TTL) * time.Second
		if res.MinTTL < 0 || ttl < res.MinTTL {
			res.MinTTL = ttl
		}

		switch {
		case ah.Type == dnsmessage.TypeA && qtype == dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return nil, err
			}
			ip := make(net.IP, net.IPv4len)
			copy(ip, r.A[:])
			res.Answers = append(res.Answers, Answer{IP: ip, TTL: ttl})
		case ah.Type == dnsmessage.TypeAAAA && qtype == dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return nil, err
			}
			ip := make(net.IP, net.IPv6len)
			copy(ip, r.AAAA[:])
			res.Answers = append(res.Answers, Answer{IP: ip, TTL: ttl})
		default:
			// CNAMEs and unrelated types still constrain MinTTL above.
			if err := p.SkipAnswer(); err != nil {
				return nil, err
			}
		}
	}

	if res.MinTTL < 0 {
		res.MinTTL = 0
	}
	return res, nil
}

// writeMsg writes a DNS message with the 2-byte big-endian length prefix
// required for DNS over TCP (RFC 1035 section 4.2.2).
func writeMsg(w io.Writer, msg []byte) error {
	if len(msg) > maxDNSMessageSize {
		return fmt.Errorf("dns: message too large (%d bytes)", len(msg))
	}
	buf := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(msg)))
	copy(buf[2:], msg)
	_, err := w.Write(buf)
	return err
}

// readMsg reads a length-prefixed DNS message.
func readMsg(r io.Reader) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(lenBuf[:])
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
