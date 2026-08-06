package dns

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/wadahiro/awsocks/internal/routing"
)

// Default tuning for a resolution rule. Exported so the config layer can
// document them and apply the same values.
const (
	// DefaultTimeout bounds a single query against a single server.
	DefaultTimeout = 3 * time.Second
	// DefaultMinTTL keeps very short TTLs from causing a tunnel round-trip
	// on nearly every dial. VPC resolvers routinely answer with 0-5s.
	DefaultMinTTL = 10 * time.Second
	// DefaultMaxTTL bounds staleness so failovers that change an address
	// (RDS, NLB) are picked up in reasonable time.
	DefaultMaxTTL = 5 * time.Minute
	// DefaultNegativeTTL is short because NXDOMAIN is often transient.
	DefaultNegativeTTL = 5 * time.Second
	// DefaultPort is appended to servers given without one.
	DefaultPort = "53"
)

// FailureMode selects what happens when resolution fails.
type FailureMode string

const (
	// FailureFallthrough passes the original hostname to the connect route,
	// preserving the behavior of not resolving at all.
	FailureFallthrough FailureMode = "fallthrough"
	// FailureFail surfaces the error to the client.
	FailureFail FailureMode = "fail"
)

// IsValid reports whether the failure mode is recognized.
func (f FailureMode) IsValid() bool {
	switch f {
	case FailureFallthrough, FailureFail:
		return true
	default:
		return false
	}
}

// Family selects which address family is preferred.
type Family string

const (
	// FamilyIPv4 queries A records only.
	FamilyIPv4 Family = "ipv4"
	// FamilyIPv6 queries AAAA first and falls back to A.
	FamilyIPv6 Family = "ipv6"
)

// IsValid reports whether the family is recognized.
func (f Family) IsValid() bool {
	switch f {
	case FamilyIPv4, FamilyIPv6:
		return true
	default:
		return false
	}
}

// Rule is one resolution rule: which names it covers, which DNS servers to
// ask, and which network route carries the query.
type Rule struct {
	// Via is the route the DNS query itself travels over. It is independent
	// of the route used to reach the resolved address: resolving a name over
	// the proxy and connecting to it directly (or the reverse) is a valid
	// configuration that only the operator can judge.
	Via routing.Route
	// Servers are DNS servers in host:port form, tried in order.
	Servers []string
	// Patterns limits the rule to matching hostnames. Empty matches all.
	Patterns []string

	Timeout     time.Duration
	MinTTL      time.Duration
	MaxTTL      time.Duration
	NegativeTTL time.Duration
	OnFailure   FailureMode
	Prefer      Family

	matchers []routing.Matcher
}

// Config is the parsed set of resolution rules, evaluated in order.
type Config struct {
	Rules []Rule
}

// Enabled reports whether any rule is configured.
func (c *Config) Enabled() bool {
	return c != nil && len(c.Rules) > 0
}

// Routes returns the distinct routes used by the rules, so callers know which
// dialers the resolver needs.
func (c *Config) Routes() []routing.Route {
	if c == nil {
		return nil
	}
	seen := make(map[routing.Route]bool)
	var out []routing.Route
	for _, r := range c.Rules {
		if !seen[r.Via] {
			seen[r.Via] = true
			out = append(out, r.Via)
		}
	}
	return out
}

// NormalizeServer validates a server address and appends the default port when
// absent. Hostnames are rejected: resolving them would require the resolver
// that is being configured.
func NormalizeServer(server string) (string, error) {
	s := strings.TrimSpace(server)
	if s == "" {
		return "", fmt.Errorf("dns: empty server address")
	}

	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	} else if ip := net.ParseIP(s); ip != nil {
		// Bare IPv6 such as "2001:db8::1" fails SplitHostPort; treat as host.
		host, port = s, ""
	}

	if net.ParseIP(host) == nil {
		return "", fmt.Errorf("dns: server %q must be an IP address, not a hostname", server)
	}

	if port == "" {
		port = DefaultPort
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("dns: server %q has an invalid port", server)
	}

	return net.JoinHostPort(host, port), nil
}
