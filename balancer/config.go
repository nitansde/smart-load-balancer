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
	// DefaultQuotaCalibration is the default background precise-quota
	// calibration interval. The usage-feedback ledger is the primary
	// quota signal and needs no polling.
	DefaultQuotaCalibration = 72 * time.Hour
	// MaxQuotaCalibration caps the background calibration interval.
	MaxQuotaCalibration = 7 * 24 * time.Hour
	// MaxPickHistory caps the sliding-window pick log so memory stays bounded
	// under extreme request rates.
	MaxPickHistory = 200000
)

// Config carries the plugin configuration decoded from the host-provided YAML.
type Config struct {
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
	// QuotaEnabled turns on background upstream quota fetching for OAuth
	// providers (currently Codex) via the host's auth callbacks. The
	// snapshots feed quota-aware ordering. Defaults to true.
	QuotaEnabled bool `yaml:"quota_enabled"`
	// QuotaProviders restricts quota fetching to these provider keys
	// (for example ["codex"]). Empty means every provider with a known
	// quota endpoint.
	QuotaProviders []string `yaml:"quota_providers"`
	// QuotaRefreshSeconds is how often precise upstream quota is re-fetched
	// as a background calibration. The primary quota signal is the usage
	// feedback ledger (usage.handle), which needs no polling; this only
	// corrects drift (e.g. quota consumed outside CPA). Zero disables the
	// background calibration entirely (pure on-demand via the management
	// UI, which routes through this plugin's quota provider). Defaults to
	// 72h.
	QuotaRefreshSeconds int `yaml:"quota_refresh_seconds"`
	// QuotaProbeFresh sends one minimal "ping" request the first time a
	// never-used quota window is selected, starting that window's
	// countdown. Defaults to true.
	QuotaProbeFresh bool `yaml:"quota_probe_fresh"`
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
	// QuotaEnabled is a plain bool; like Sticky it cannot distinguish
	// "unset" from false, so the default (true) lives in DefaultConfig.
	// QuotaRefreshSeconds: 0 disables background calibration (on-demand
	// only); negative means the default (72h).
	if out.QuotaRefreshSeconds < 0 {
		out.QuotaRefreshSeconds = int(DefaultQuotaCalibration / time.Second)
	}
	if out.QuotaRefreshSeconds > 0 && out.QuotaRefreshSeconds < 3600 {
		out.QuotaRefreshSeconds = 3600
	}
	if out.QuotaRefreshSeconds > int(MaxQuotaCalibration/time.Second) {
		out.QuotaRefreshSeconds = int(MaxQuotaCalibration / time.Second)
	}
	// QuotaProbeFresh defaults to true; like Sticky the default lives in
	// DefaultConfig because YAML cannot distinguish unset from false.
	quotaProviders := make([]string, 0, len(out.QuotaProviders))
	seenQP := make(map[string]struct{}, len(out.QuotaProviders))
	for _, p := range out.QuotaProviders {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if _, ok := seenQP[p]; ok {
			continue
		}
		seenQP[p] = struct{}{}
		quotaProviders = append(quotaProviders, p)
	}
	out.QuotaProviders = quotaProviders
	return out
}

// DefaultConfig returns the recommended configuration.
func DefaultConfig() Config {
	return Config{
		Strategy:              DefaultStrategy,
		Sticky:                DefaultSticky,
		StickyTTLSeconds:      int(DefaultStickyTTL / time.Second),
		WindowSeconds:         int(DefaultWindow / time.Second),
		MaxInflightPerProfile: DefaultMaxInflightPerProfile,
		QuotaEnabled:          true,
		QuotaRefreshSeconds:   int(DefaultQuotaCalibration / time.Second),
		QuotaProbeFresh:       true,
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
