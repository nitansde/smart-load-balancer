// Package balancer implements the Smart Load Balancer routing core for CLIProxyAPI.
//
// It spreads concurrent client requests across upstream auth profiles (for
// example Codex profiles) instead of piling them onto the profile the default
// scheduler would pick. Per-client stickiness keeps prompt caches warm while
// a least-connections policy with deterministic per-key tie-breaking spreads
// simultaneous requests from different API keys across profiles.
package balancer

import (
	"fmt"
	"strings"
	"time"
)

// Strategies supported by the balancer.
const (
	StrategyLeastConnections = "least-connections"
	StrategyRoundRobin       = "round-robin"
)

// Defaults applied when a config field is unset.
const (
	DefaultStrategy              = StrategyLeastConnections
	DefaultSticky                = true
	DefaultStickyTTL             = 24 * time.Hour
	DefaultWindow                = 120 * time.Second
	DefaultMaxInflightPerProfile = 8
	// MaxPickHistory caps the sliding-window pick log so memory stays bounded
	// under extreme request rates.
	MaxPickHistory = 200000
)

// Config carries the plugin configuration decoded from the host-provided YAML.
type Config struct {
	// Enabled is the master switch. When false the plugin declines every
	// scheduler pick (the host falls back to its default scheduler) and
	// usage/quota handling becomes inert. Defaults to true.
	Enabled bool `yaml:"enabled"`
	// Providers optionally restricts balancing to these provider keys
	// (for example ["codex"]). Empty means every provider offered by the host.
	Providers []string `yaml:"providers"`
	// Strategy is accepted for compatibility ("least-connections",
	// "round-robin") but ignored: the plugin always takes the
	// top-ranked candidate from its quota-aware ordering.
	Strategy string `yaml:"strategy"`
	// Sticky pins a client API key to one profile while that profile stays
	// healthy, improving prompt-cache reuse. Defaults to true.
	Sticky bool `yaml:"sticky"`
	// StickyTTLSeconds bounds how long an idle sticky assignment is kept.
	StickyTTLSeconds int `yaml:"sticky_ttl_seconds"`
	// WindowSeconds is the sliding window used to estimate recent load per
	// profile. Concurrent picks inside the window steer new requests away
	// from busy profiles.
	WindowSeconds int `yaml:"window_seconds"`
	// MaxInflightPerProfile is the recent-pick threshold above which a sticky
	// assignment spills over to the least-loaded profile.
	MaxInflightPerProfile int `yaml:"max_inflight_per_profile"`
	// QuotaPriorities is an ordered list of auth profile IDs, most
	// preferred first. It applies inside quota-aware ordering; profiles
	// not listed rank after all listed ones.
	QuotaPriorities []string `yaml:"quota_priorities"`
}

// WithDefaults returns the config with zero values replaced by defaults and
// all values normalized and clamped to sane ranges.
func (c Config) WithDefaults() Config {
	out := c
	if strings.TrimSpace(out.Strategy) == "" {
		out.Strategy = DefaultStrategy
	}
	out.Strategy = strings.ToLower(strings.TrimSpace(out.Strategy))

	// sticky defaults to true; there is no way to distinguish "unset" from
	// false via plain YAML decoding, so callers that need the default should
	// use DefaultConfig as the base. WithDefaults preserves explicit false.
	providers := make([]string, 0, len(out.Providers))
	seen := make(map[string]struct{}, len(out.Providers))
	for _, p := range out.Providers {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		providers = append(providers, p)
	}
	out.Providers = providers

	if out.StickyTTLSeconds <= 0 {
		out.StickyTTLSeconds = int(DefaultStickyTTL / time.Second)
	}
	if out.WindowSeconds <= 0 {
		out.WindowSeconds = int(DefaultWindow / time.Second)
	}
	if out.WindowSeconds > 3600 {
		out.WindowSeconds = 3600
	}
	if out.MaxInflightPerProfile <= 0 {
		out.MaxInflightPerProfile = DefaultMaxInflightPerProfile
	}
	if out.MaxInflightPerProfile > 10000 {
		out.MaxInflightPerProfile = 10000
	}
	return out
}

// DefaultConfig returns the recommended configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:               true,
		Strategy:              DefaultStrategy,
		Sticky:                DefaultSticky,
		StickyTTLSeconds:      int(DefaultStickyTTL / time.Second),
		WindowSeconds:         int(DefaultWindow / time.Second),
		MaxInflightPerProfile: DefaultMaxInflightPerProfile,
	}.WithDefaults()
}

// Validate reports whether the strategy name is supported.
func (c Config) Validate() error {
	switch strings.ToLower(strings.TrimSpace(c.Strategy)) {
	case StrategyLeastConnections, StrategyRoundRobin:
		return nil
	default:
		return fmt.Errorf("unknown strategy %q: want %q or %q", c.Strategy, StrategyLeastConnections, StrategyRoundRobin)
	}
}

// Window returns the sliding-window duration.
func (c Config) Window() time.Duration {
	return time.Duration(c.WithDefaults().WindowSeconds) * time.Second
}

// StickyTTL returns the sticky-assignment TTL.
func (c Config) StickyTTL() time.Duration {
	return time.Duration(c.WithDefaults().StickyTTLSeconds) * time.Second
}
