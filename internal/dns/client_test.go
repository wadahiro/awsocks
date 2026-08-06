package dns

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/wadahiro/awsocks/internal/testutil/fakedns"
)

// netDial dials the fake server over real TCP, standing in for the tunnel.
func netDial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

func startFake(t *testing.T) (*fakedns.Server, string) {
	t.Helper()
	srv := fakedns.NewServer()
	addr, err := srv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return srv, addr
}

func TestQueryReturnsARecordWithTTL(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 300,
	})

	c := NewClient(netDial, []string{addr}, 3*time.Second)
	res, err := c.Query(context.Background(), "app.internal.example.com", dnsmessage.TypeA)

	require.NoError(t, err)
	require.Len(t, res.Answers, 1)
	assert.Equal(t, "10.0.0.5", res.Answers[0].IP.String())
	assert.Equal(t, 300*time.Second, res.Answers[0].TTL)
	assert.Equal(t, 300*time.Second, res.MinTTL)
	assert.Equal(t, dnsmessage.RCodeSuccess, res.Rcode)
}

func TestQueryReturnsAAAARecord(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("v6.internal.example.com", fakedns.Record{
		AAAA: []net.IP{net.ParseIP("2001:db8::1")},
		TTL:  60,
	})

	c := NewClient(netDial, []string{addr}, 3*time.Second)
	res, err := c.Query(context.Background(), "v6.internal.example.com", dnsmessage.TypeAAAA)

	require.NoError(t, err)
	require.Len(t, res.Answers, 1)
	assert.Equal(t, "2001:db8::1", res.Answers[0].IP.String())
}

func TestQueryReturnsMultipleAnswersInOrder(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("multi.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.2")},
		TTL: 120,
	})

	c := NewClient(netDial, []string{addr}, 3*time.Second)
	res, err := c.Query(context.Background(), "multi.internal.example.com", dnsmessage.TypeA)

	require.NoError(t, err)
	require.Len(t, res.Answers, 2)
	assert.Equal(t, "10.0.0.1", res.Answers[0].IP.String())
	assert.Equal(t, "10.0.0.2", res.Answers[1].IP.String())
}

func TestQueryNXDOMAINIsNotAnError(t *testing.T) {
	_, addr := startFake(t)

	c := NewClient(netDial, []string{addr}, 3*time.Second)
	res, err := c.Query(context.Background(), "missing.internal.example.com", dnsmessage.TypeA)

	require.NoError(t, err)
	assert.Equal(t, dnsmessage.RCodeNameError, res.Rcode)
	assert.Empty(t, res.Answers)
}

func TestQueryEmptyAnswerIsNotAnError(t *testing.T) {
	srv, addr := startFake(t)
	// Record exists but has no A records.
	srv.SetRecord("noa.internal.example.com", fakedns.Record{
		AAAA: []net.IP{net.ParseIP("2001:db8::1")},
		TTL:  60,
	})

	c := NewClient(netDial, []string{addr}, 3*time.Second)
	res, err := c.Query(context.Background(), "noa.internal.example.com", dnsmessage.TypeA)

	require.NoError(t, err)
	assert.Equal(t, dnsmessage.RCodeSuccess, res.Rcode)
	assert.Empty(t, res.Answers)
}

func TestQueryTimesOutWhenServerDoesNotRespond(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("slow.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.9")},
		TTL: 60,
	})
	srv.Delay = 2 * time.Second

	c := NewClient(netDial, []string{addr}, 100*time.Millisecond)
	start := time.Now()
	_, err := c.Query(context.Background(), "slow.internal.example.com", dnsmessage.TypeA)

	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "should give up well before the server responds")
}

func TestQueryFailsWhenServerClosesWithoutResponding(t *testing.T) {
	srv, addr := startFake(t)
	srv.DropQuery = true

	c := NewClient(netDial, []string{addr}, time.Second)
	_, err := c.Query(context.Background(), "app.internal.example.com", dnsmessage.TypeA)

	require.Error(t, err)
}

func TestQueryFallsBackToSecondServer(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})

	// First server address is closed, so dialing it fails.
	dead := deadAddr(t)

	c := NewClient(netDial, []string{dead, addr}, time.Second)
	res, err := c.Query(context.Background(), "app.internal.example.com", dnsmessage.TypeA)

	require.NoError(t, err)
	require.Len(t, res.Answers, 1)
	assert.Equal(t, "10.0.0.5", res.Answers[0].IP.String())
}

func TestQueryFailsWhenAllServersAreUnreachable(t *testing.T) {
	c := NewClient(netDial, []string{deadAddr(t), deadAddr(t)}, 500*time.Millisecond)
	_, err := c.Query(context.Background(), "app.internal.example.com", dnsmessage.TypeA)

	require.Error(t, err)
}

func TestQueryRejectsOverlongName(t *testing.T) {
	_, addr := startFake(t)
	long := strings.Repeat("a", 300) + ".example.com"

	c := NewClient(netDial, []string{addr}, time.Second)
	_, err := c.Query(context.Background(), long, dnsmessage.TypeA)

	require.Error(t, err)
}

func TestQueryHonorsContextCancellation(t *testing.T) {
	srv, addr := startFake(t)
	srv.Delay = 2 * time.Second
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.5")},
		TTL: 60,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	c := NewClient(netDial, []string{addr}, 5*time.Second)
	start := time.Now()
	_, err := c.Query(ctx, "app.internal.example.com", dnsmessage.TypeA)

	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
}

func TestQueryUsesMinimumTTLAcrossAnswers(t *testing.T) {
	srv, addr := startFake(t)
	srv.SetRecord("app.internal.example.com", fakedns.Record{
		A:   []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.2")},
		TTL: 45,
	})

	c := NewClient(netDial, []string{addr}, time.Second)
	res, err := c.Query(context.Background(), "app.internal.example.com", dnsmessage.TypeA)

	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, res.MinTTL)
}

// deadAddr returns an address that nothing is listening on.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}
