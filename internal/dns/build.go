package dns

import (
	"fmt"
	"time"

	"github.com/wadahiro/awsocks/internal/routing"
)

// RuleSpec is the unparsed form of a resolution rule, as it appears in
// configuration. BuildConfig turns it into a Rule.
type RuleSpec struct {
	Via         string
	Servers     []string
	Patterns    []string
	Timeout     string
	MinTTL      string
	MaxTTL      string
	NegativeTTL string
	OnFailure   string
	Prefer      string
}

// BuildConfig validates specs and converts them into a runtime Config.
// Returns nil when no rules are given, meaning resolution is disabled.
//
// Validation is done up front so a malformed rule fails at startup rather
// than silently misbehaving on the first connection.
func BuildConfig(specs []RuleSpec) (*Config, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	cfg := &Config{Rules: make([]Rule, 0, len(specs))}

	for i, spec := range specs {
		rule, err := buildRule(spec)
		if err != nil {
			return nil, fmt.Errorf("dns rule %d: %w", i+1, err)
		}
		cfg.Rules = append(cfg.Rules, rule)
	}

	return cfg, nil
}

func buildRule(spec RuleSpec) (Rule, error) {
	rule := Rule{
		Patterns:    spec.Patterns,
		Timeout:     DefaultTimeout,
		MinTTL:      DefaultMinTTL,
		MaxTTL:      DefaultMaxTTL,
		NegativeTTL: DefaultNegativeTTL,
		OnFailure:   FailureFallthrough,
		Prefer:      FamilyIPv4,
	}

	// Via defaults to proxy: resolving through the tunnel is the case that
	// the host's own resolver cannot cover.
	rule.Via = routing.RouteProxy
	if spec.Via != "" {
		via := routing.Route(spec.Via)
		if !via.IsValid() {
			return rule, fmt.Errorf("invalid via %q (want proxy, direct, or vm-direct)", spec.Via)
		}
		rule.Via = via
	}

	if len(spec.Servers) == 0 {
		return rule, fmt.Errorf("at least one server is required")
	}
	rule.Servers = make([]string, 0, len(spec.Servers))
	for _, s := range spec.Servers {
		normalized, err := NormalizeServer(s)
		if err != nil {
			return rule, err
		}
		rule.Servers = append(rule.Servers, normalized)
	}

	var err error
	if rule.Timeout, err = parseDuration(spec.Timeout, DefaultTimeout, "timeout"); err != nil {
		return rule, err
	}
	if rule.MinTTL, err = parseDuration(spec.MinTTL, DefaultMinTTL, "min-ttl"); err != nil {
		return rule, err
	}
	if rule.MaxTTL, err = parseDuration(spec.MaxTTL, DefaultMaxTTL, "max-ttl"); err != nil {
		return rule, err
	}
	if rule.NegativeTTL, err = parseDuration(spec.NegativeTTL, DefaultNegativeTTL, "negative-ttl"); err != nil {
		return rule, err
	}
	if rule.MaxTTL > 0 && rule.MinTTL > rule.MaxTTL {
		return rule, fmt.Errorf("min-ttl (%s) must not exceed max-ttl (%s)", rule.MinTTL, rule.MaxTTL)
	}

	if spec.OnFailure != "" {
		mode := FailureMode(spec.OnFailure)
		if !mode.IsValid() {
			return rule, fmt.Errorf("invalid on-failure %q (want fallthrough or fail)", spec.OnFailure)
		}
		rule.OnFailure = mode
	}

	if spec.Prefer != "" {
		family := Family(spec.Prefer)
		if !family.IsValid() {
			return rule, fmt.Errorf("invalid prefer %q (want ipv4 or ipv6)", spec.Prefer)
		}
		rule.Prefer = family
	}

	return rule, nil
}

func parseDuration(value string, fallback time.Duration, field string) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", field, value)
	}
	return d, nil
}
