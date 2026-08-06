package dns

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wadahiro/awsocks/internal/routing"
)

func TestBuildConfigReturnsNilWhenNoRules(t *testing.T) {
	cfg, err := BuildConfig(nil)

	require.NoError(t, err)
	assert.Nil(t, cfg)
	assert.False(t, cfg.Enabled())
}

func TestBuildConfigAppliesDefaults(t *testing.T) {
	cfg, err := BuildConfig([]RuleSpec{{Servers: []string{"10.0.0.2"}}})

	require.NoError(t, err)
	require.Len(t, cfg.Rules, 1)

	r := cfg.Rules[0]
	assert.Equal(t, routing.RouteProxy, r.Via, "via defaults to proxy")
	assert.Equal(t, []string{"10.0.0.2:53"}, r.Servers, "port defaults to 53")
	assert.Equal(t, DefaultTimeout, r.Timeout)
	assert.Equal(t, DefaultMinTTL, r.MinTTL)
	assert.Equal(t, DefaultMaxTTL, r.MaxTTL)
	assert.Equal(t, DefaultNegativeTTL, r.NegativeTTL)
	assert.Equal(t, FailureFallthrough, r.OnFailure)
	assert.Equal(t, FamilyIPv4, r.Prefer)
}

func TestBuildConfigParsesAllFields(t *testing.T) {
	cfg, err := BuildConfig([]RuleSpec{{
		Via:         "vm-direct",
		Servers:     []string{"10.0.0.2:5353", "2001:db8::1"},
		Patterns:    []string{"*.internal.example.com"},
		Timeout:     "7s",
		MinTTL:      "20s",
		MaxTTL:      "10m",
		NegativeTTL: "1s",
		OnFailure:   "fail",
		Prefer:      "ipv6",
	}})

	require.NoError(t, err)
	r := cfg.Rules[0]
	assert.Equal(t, routing.RouteVMDirect, r.Via)
	assert.Equal(t, []string{"10.0.0.2:5353", "[2001:db8::1]:53"}, r.Servers)
	assert.Equal(t, []string{"*.internal.example.com"}, r.Patterns)
	assert.Equal(t, 7*time.Second, r.Timeout)
	assert.Equal(t, 20*time.Second, r.MinTTL)
	assert.Equal(t, 10*time.Minute, r.MaxTTL)
	assert.Equal(t, time.Second, r.NegativeTTL)
	assert.Equal(t, FailureFail, r.OnFailure)
	assert.Equal(t, FamilyIPv6, r.Prefer)
}

func TestBuildConfigRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		spec RuleSpec
	}{
		{
			name: "no servers",
			spec: RuleSpec{Patterns: []string{"*.example.com"}},
		},
		{
			name: "hostname as server",
			spec: RuleSpec{Servers: []string{"dns.example.com"}},
		},
		{
			name: "invalid via",
			spec: RuleSpec{Servers: []string{"10.0.0.2"}, Via: "tunnel"},
		},
		{
			name: "invalid on-failure",
			spec: RuleSpec{Servers: []string{"10.0.0.2"}, OnFailure: "retry"},
		},
		{
			name: "invalid prefer",
			spec: RuleSpec{Servers: []string{"10.0.0.2"}, Prefer: "ipv5"},
		},
		{
			name: "unparseable timeout",
			spec: RuleSpec{Servers: []string{"10.0.0.2"}, Timeout: "soon"},
		},
		{
			name: "negative timeout",
			spec: RuleSpec{Servers: []string{"10.0.0.2"}, Timeout: "-1s"},
		},
		{
			name: "min-ttl above max-ttl",
			spec: RuleSpec{Servers: []string{"10.0.0.2"}, MinTTL: "10m", MaxTTL: "1m"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildConfig([]RuleSpec{tt.spec})
			require.Error(t, err)
		})
	}
}

func TestBuildConfigReportsRuleIndex(t *testing.T) {
	_, err := BuildConfig([]RuleSpec{
		{Servers: []string{"10.0.0.2"}},
		{Servers: []string{"not-an-ip"}},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rule 2", "error should identify which rule failed")
}

func TestConfigRoutesReturnsDistinctRoutes(t *testing.T) {
	cfg, err := BuildConfig([]RuleSpec{
		{Servers: []string{"10.0.0.2"}, Via: "proxy"},
		{Servers: []string{"10.0.0.3"}, Via: "direct"},
		{Servers: []string{"10.0.0.4"}, Via: "proxy"},
	})
	require.NoError(t, err)

	routes := cfg.Routes()

	assert.ElementsMatch(t, []routing.Route{routing.RouteProxy, routing.RouteDirect}, routes)
}
