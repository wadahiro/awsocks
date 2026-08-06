package dns

import (
	"context"
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sync/singleflight"

	"github.com/wadahiro/awsocks/internal/clock"
	"github.com/wadahiro/awsocks/internal/log"
	"github.com/wadahiro/awsocks/internal/routing"
)

var logger = log.For(log.ComponentDNS)

// Resolver resolves hostnames using the configured rules, caching results and
// collapsing concurrent lookups for the same name.
//
// A nil *Resolver is valid and resolves nothing, so callers can treat the
// feature as disabled without a nil check at every call site.
type Resolver struct {
	rules   []Rule
	clients []*Client
	caches  []*Cache
	sf      singleflight.Group
}

// NewResolver builds a resolver from cfg. dialers must contain an entry for
// every route referenced by a rule's Via; a missing one is a configuration
// error rather than a runtime failure, so it is reported here.
func NewResolver(cfg *Config, dialers map[routing.Route]DialFunc, clk clock.Clock) (*Resolver, error) {
	if cfg == nil || len(cfg.Rules) == 0 {
		return &Resolver{}, nil
	}

	r := &Resolver{}

	for i := range cfg.Rules {
		rule := cfg.Rules[i]

		dial, ok := dialers[rule.Via]
		if !ok || dial == nil {
			return nil, fmt.Errorf("dns: no dialer available for via=%q (rule %d)", rule.Via, i+1)
		}
		if len(rule.Servers) == 0 {
			return nil, fmt.Errorf("dns: rule %d has no servers", i+1)
		}

		rule.matchers = make([]routing.Matcher, 0, len(rule.Patterns))
		for _, p := range rule.Patterns {
			rule.matchers = append(rule.matchers, routing.ParseMatcher(p))
		}

		r.rules = append(r.rules, rule)
		r.clients = append(r.clients, NewClient(dial, rule.Servers, rule.Timeout))
		r.caches = append(r.caches, NewCache(clk, rule.MinTTL, rule.MaxTTL, rule.NegativeTTL, defaultMaxCacheEntries))
	}

	return r, nil
}

// Enabled reports whether any rule is configured.
func (r *Resolver) Enabled() bool {
	return r != nil && len(r.rules) > 0
}

// Resolve looks up host using the first matching rule.
//
// The boolean reports whether the caller should dial the returned IP. It is
// false when nothing applies (no rule matches, host is already an IP) and when
// resolution failed under the fallthrough policy, in which case the caller
// keeps using the original hostname. An error is returned only when a rule
// sets OnFailure to fail.
func (r *Resolver) Resolve(ctx context.Context, host string) (net.IP, bool, error) {
	if !r.Enabled() {
		return nil, false, nil
	}

	// An address literal needs no resolution, and querying for it would only
	// produce NXDOMAIN.
	if ip := net.ParseIP(host); ip != nil {
		return nil, false, nil
	}

	idx := r.matchRule(host)
	if idx < 0 {
		return nil, false, nil
	}

	rule := &r.rules[idx]
	cache := r.caches[idx]
	key := strings.ToLower(host)

	if e, ok := cache.Get(key); ok {
		if e.negative {
			return r.onFailure(rule, host, fmt.Errorf("dns: %s not found (cached)", host))
		}
		if ip := pickIP(e, rule.Prefer); ip != nil {
			return ip, true, nil
		}
	}

	// The shared query deliberately does not inherit the caller's context:
	// singleflight hands one lookup to every waiter, so a cancelled first
	// caller would otherwise abort the lookup for everyone still waiting.
	// The per-rule timeout in Client bounds it instead.
	v, err, _ := r.sf.Do(fmt.Sprintf("%d\x00%s", idx, key), func() (any, error) {
		return r.lookup(context.WithoutCancel(ctx), idx, key)
	})

	if err != nil {
		logger.Debug("DNS lookup failed", "host", host, "via", rule.Via, "error", err)
		return r.onFailure(rule, host, err)
	}

	e, _ := v.(*entry)
	if e == nil || e.negative {
		return r.onFailure(rule, host, fmt.Errorf("dns: %s not found", host))
	}

	ip := pickIP(e, rule.Prefer)
	if ip == nil {
		return r.onFailure(rule, host, fmt.Errorf("dns: %s has no usable address", host))
	}

	logger.Debug("Host resolved via DNS", "host", host, "ip", ip, "via", rule.Via)
	return ip, true, nil
}

// Invalidate drops any cached result for host across all rules. Callers use it
// when a resolved address turns out to be unreachable, so a changed address
// (failover, rescheduling) is picked up on the next attempt instead of after
// the TTL.
func (r *Resolver) Invalidate(host string) {
	if !r.Enabled() {
		return
	}
	for _, c := range r.caches {
		c.Delete(host)
	}
}

// lookup performs the queries for one rule and caches the outcome.
func (r *Resolver) lookup(ctx context.Context, idx int, host string) (*entry, error) {
	rule := &r.rules[idx]
	client := r.clients[idx]
	cache := r.caches[idx]

	var ipv4, ipv6 []net.IP
	ttl := rule.MinTTL
	found := false

	query := func(qtype dnsmessage.Type) (*Result, error) {
		return client.Query(ctx, host, qtype)
	}

	if rule.Prefer == FamilyIPv6 {
		res, err := query(dnsmessage.TypeAAAA)
		if err != nil {
			return nil, err
		}
		if len(res.Answers) > 0 {
			ipv6 = answersToIPs(res.Answers)
			ttl = res.MinTTL
			found = true
		}
	}

	// A is queried unless AAAA already answered, so the common IPv4 case costs
	// a single round trip over the tunnel.
	if !found {
		res, err := query(dnsmessage.TypeA)
		if err != nil {
			return nil, err
		}
		if len(res.Answers) > 0 {
			ipv4 = answersToIPs(res.Answers)
			ttl = res.MinTTL
			found = true
		}
	}

	if !found {
		cache.PutNegative(host)
		return &entry{negative: true}, nil
	}

	cache.Put(host, ipv4, ipv6, ttl)
	e, ok := cache.Get(host)
	if !ok {
		// Should not happen, but return a usable value rather than nothing.
		return &entry{ipv4: ipv4, ipv6: ipv6}, nil
	}
	return e, nil
}

// matchRule returns the index of the first rule covering host, or -1.
func (r *Resolver) matchRule(host string) int {
	for i := range r.rules {
		rule := &r.rules[i]
		if len(rule.matchers) == 0 {
			return i
		}
		for _, m := range rule.matchers {
			if m.Match(host) {
				return i
			}
		}
	}
	return -1
}

// onFailure applies the rule's failure policy.
func (r *Resolver) onFailure(rule *Rule, host string, cause error) (net.IP, bool, error) {
	if rule.OnFailure == FailureFail {
		return nil, false, fmt.Errorf("dns: resolving %s: %w", host, cause)
	}
	// Fallthrough: the caller keeps the hostname, preserving the behavior of
	// not having configured DNS resolution at all.
	return nil, false, nil
}

// pickIP selects an address from the entry honoring the family preference,
// falling back to the other family when the preferred one is absent.
func pickIP(e *entry, prefer Family) net.IP {
	if prefer == FamilyIPv6 {
		if len(e.ipv6) > 0 {
			return e.ipv6[0]
		}
		if len(e.ipv4) > 0 {
			return e.ipv4[0]
		}
		return nil
	}
	if len(e.ipv4) > 0 {
		return e.ipv4[0]
	}
	if len(e.ipv6) > 0 {
		return e.ipv6[0]
	}
	return nil
}

func answersToIPs(answers []Answer) []net.IP {
	out := make([]net.IP, 0, len(answers))
	for _, a := range answers {
		out = append(out, a.IP)
	}
	return out
}
