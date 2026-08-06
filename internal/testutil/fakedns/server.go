// Package fakedns provides a minimal DNS-over-TCP server for tests.
package fakedns

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Record holds the answers served for a single name.
type Record struct {
	A    []net.IP
	AAAA []net.IP
	TTL  uint32
}

// Query records a question received by the server.
type Query struct {
	Name string
	Type dnsmessage.Type
}

// Server is a DNS-over-TCP server backed by a static record table.
// The zero value is not usable; call NewServer.
type Server struct {
	listener net.Listener

	mu      sync.Mutex
	records map[string]Record // lowercase name without trailing dot
	queries []Query

	// Delay is applied before writing each response.
	Delay time.Duration
	// DropQuery closes the connection without responding when true.
	DropQuery bool
	// ForceRcode overrides the response code when non-zero.
	ForceRcode dnsmessage.RCode

	wg     sync.WaitGroup
	closed chan struct{}
}

// NewServer creates a server with an empty record table.
func NewServer() *Server {
	return &Server{
		records: make(map[string]Record),
		closed:  make(chan struct{}),
	}
}

// Start listens on a loopback port and serves until Close.
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	s.listener = ln

	s.wg.Add(1)
	go s.serve()

	return ln.Addr().String(), nil
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func (s *Server) handle(conn net.Conn) {
	for {
		msg, err := readMsg(conn)
		if err != nil {
			return
		}

		var p dnsmessage.Parser
		hdr, err := p.Start(msg)
		if err != nil {
			return
		}
		q, err := p.Question()
		if err != nil {
			return
		}

		name := trimDot(q.Name.String())
		s.mu.Lock()
		s.queries = append(s.queries, Query{Name: name, Type: q.Type})
		rec, found := s.records[lower(name)]
		delay := s.Delay
		drop := s.DropQuery
		forced := s.ForceRcode
		s.mu.Unlock()

		if drop {
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}

		resp, err := buildResponse(hdr.ID, q, rec, found, forced)
		if err != nil {
			return
		}
		if err := writeMsg(conn, resp); err != nil {
			return
		}
	}
}

func buildResponse(id uint16, q dnsmessage.Question, rec Record, found bool, forced dnsmessage.RCode) ([]byte, error) {
	rcode := dnsmessage.RCodeSuccess
	if forced != dnsmessage.RCodeSuccess {
		rcode = forced
	} else if !found {
		rcode = dnsmessage.RCodeNameError
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:            id,
		Response:      true,
		Authoritative: true,
		RCode:         rcode,
	})
	b.EnableCompression()

	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}

	if found && rcode == dnsmessage.RCodeSuccess {
		hdr := dnsmessage.ResourceHeader{
			Name:  q.Name,
			Class: dnsmessage.ClassINET,
			TTL:   rec.TTL,
		}
		switch q.Type {
		case dnsmessage.TypeA:
			for _, ip := range rec.A {
				v4 := ip.To4()
				if v4 == nil {
					continue
				}
				var a [4]byte
				copy(a[:], v4)
				if err := b.AResource(hdr, dnsmessage.AResource{A: a}); err != nil {
					return nil, err
				}
			}
		case dnsmessage.TypeAAAA:
			for _, ip := range rec.AAAA {
				v6 := ip.To16()
				if v6 == nil {
					continue
				}
				var a [16]byte
				copy(a[:], v6)
				if err := b.AAAAResource(hdr, dnsmessage.AAAAResource{AAAA: a}); err != nil {
					return nil, err
				}
			}
		}
	}

	return b.Finish()
}

// SetRecord installs the answers served for name.
func (s *Server) SetRecord(name string, rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[lower(trimDot(name))] = rec
}

// Queries returns a copy of the questions received so far.
func (s *Server) Queries() []Query {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Query, len(s.queries))
	copy(out, s.queries)
	return out
}

// QueryCount returns how many questions the server has received.
func (s *Server) QueryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queries)
}

// ResetQueries clears the recorded questions.
func (s *Server) ResetQueries() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = nil
}

// Close stops the server and waits for handlers to finish.
func (s *Server) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
		close(s.closed)
	}
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	s.wg.Wait()
	return err
}

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

func writeMsg(w io.Writer, msg []byte) error {
	buf := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(msg)))
	copy(buf[2:], msg)
	_, err := w.Write(buf)
	return err
}

func trimDot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
