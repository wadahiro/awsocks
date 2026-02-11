package routing

import (
	"net"
	"strings"
)

// Matcher is an interface for matching hosts/addresses
type Matcher interface {
	Match(host string) bool
}

// DomainMatcher matches domain names with glob patterns
// Supports:
// - *.example.com (matches foo.example.com, bar.baz.example.com)
// - example.com (exact match)
type DomainMatcher struct {
	pattern string
	prefix  string // For *.domain patterns
	exact   string // For exact domain matches
}

// NewDomainMatcher creates a new domain matcher
func NewDomainMatcher(pattern string) *DomainMatcher {
	m := &DomainMatcher{pattern: pattern}

	if strings.HasPrefix(pattern, "*.") {
		// *.example.com -> matches anything ending with .example.com
		m.prefix = strings.TrimPrefix(pattern, "*")
	} else if strings.Contains(pattern, "*") {
		// Other wildcard patterns - store as-is for more complex matching
		m.prefix = ""
		m.exact = ""
	} else {
		// Exact match
		m.exact = strings.ToLower(pattern)
	}

	return m
}

// Match checks if the host matches this pattern
func (m *DomainMatcher) Match(host string) bool {
	host = strings.ToLower(host)

	// Exact match
	if m.exact != "" {
		return host == m.exact
	}

	// Wildcard suffix match (*.example.com)
	if m.prefix != "" {
		suffix := strings.ToLower(m.prefix)
		// Match both ".example.com" suffix and "example.com" itself
		if strings.HasSuffix(host, suffix) {
			return true
		}
		// Also match exact domain (*.example.com should match example.com)
		if host == strings.TrimPrefix(suffix, ".") {
			return true
		}
		return false
	}

	// Generic glob pattern (not commonly used)
	return globMatch(m.pattern, host)
}

// globMatch performs simple glob pattern matching
func globMatch(pattern, str string) bool {
	pattern = strings.ToLower(pattern)
	str = strings.ToLower(str)

	for len(pattern) > 0 {
		if pattern[0] == '*' {
			// Skip consecutive asterisks
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			// Try matching the rest of the pattern at each position
			for i := 0; i <= len(str); i++ {
				if globMatch(pattern, str[i:]) {
					return true
				}
			}
			return false
		}

		if len(str) == 0 || (pattern[0] != '?' && pattern[0] != str[0]) {
			return false
		}
		pattern = pattern[1:]
		str = str[1:]
	}
	return len(str) == 0
}

// CIDRMatcher matches IP addresses against CIDR ranges
type CIDRMatcher struct {
	network *net.IPNet
}

// NewCIDRMatcher creates a new CIDR matcher
func NewCIDRMatcher(cidr string) (*CIDRMatcher, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	return &CIDRMatcher{network: network}, nil
}

// Match checks if the host (IP address) is within the CIDR range
func (m *CIDRMatcher) Match(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return m.network.Contains(ip)
}

// ExactMatcher matches exact strings (for localhost, etc.)
type ExactMatcher struct {
	value string
}

// NewExactMatcher creates a new exact matcher
func NewExactMatcher(value string) *ExactMatcher {
	return &ExactMatcher{value: strings.ToLower(value)}
}

// Match checks if the host exactly matches
func (m *ExactMatcher) Match(host string) bool {
	return strings.ToLower(host) == m.value
}

// ParseMatcher creates an appropriate matcher based on the pattern
func ParseMatcher(pattern string) Matcher {
	// Check if it's a CIDR notation
	if strings.Contains(pattern, "/") {
		m, err := NewCIDRMatcher(pattern)
		if err == nil {
			return m
		}
		// If CIDR parsing fails, treat as domain pattern
	}

	// Check if it contains wildcards -> domain pattern
	if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") {
		return NewDomainMatcher(pattern)
	}

	// Check if it's an IP address without CIDR
	if net.ParseIP(pattern) != nil {
		// Single IP - create a /32 or /128 CIDR
		if strings.Contains(pattern, ":") {
			m, _ := NewCIDRMatcher(pattern + "/128")
			return m
		}
		m, _ := NewCIDRMatcher(pattern + "/32")
		return m
	}

	// Treat as exact domain match
	return NewExactMatcher(pattern)
}
