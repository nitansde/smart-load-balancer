package balancer

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Candidate is one upstream auth profile offered by the host for a pick.
type Candidate struct {
	ID       string
	Provider string
	// Priority is the host priority tier. Higher wins, matching CPA's
	// default scheduler which serves higher priority numbers first.
	Priority int
	// Status is the host-visible auth status.
	Status string
}

// QuotaInfo is the quota view of one profile used for pick ordering.
type QuotaInfo struct {
	// UsedPercent is the precise upstream usage (0-100) when a snapshot
	// exists (e.g. fetched on demand through the quota provider).
	UsedPercent *float64
	// ConsumedTokens is the estimated consumption from usage feedback.
	ConsumedTokens int64
	// Known means the profile takes part in quota-aware ordering: it has
	// a snapshot, a usage ledger entry, or belongs to a provider with a
	// known quota endpoint (a never-used profile is 100% remaining).
	Known bool
	// BlockedUntil excludes the profile while it is in the future
	// (upstream reported the quota exhausted).
	BlockedUntil time.Time
	// Fresh means never observed and no snapshot.
	Fresh bool
}

// QuotaResolver supplies quota info per profile. A nil resolver reports
// zero QuotaInfo for every profile.
type QuotaResolver interface {
	Lookup(authID, provider string) QuotaInfo
}

// pickRecord remembers a routing decision inside the sliding window.
type pickRecord struct {
	authID string
	at     time.Time
}

// stickyEntry pins a client key hash to one profile.
type stickyEntry struct {
	authID   string
	lastUsed time.Time
}

// Balancer spreads picks across candidates. It is safe for concurrent use;
// the host may invoke scheduler.pick from many goroutines at once.
type Balancer struct {
	mu       sync.Mutex
	now      func() time.Time
	picks    []pickRecord
	sticky   map[string]stickyEntry
	rrCursor uint64
}

// New returns a Balancer using the real clock.
func New() *Balancer {
	return &Balancer{now: time.Now, sticky: make(map[string]stickyEntry)}
}

// newWithClock is used by tests to control time.
func newWithClock(now func() time.Time) *Balancer {
	return &Balancer{now: now, sticky: make(map[string]stickyEntry)}
}

// ClientKeyHash derives a stable, non-reversible identity for the calling
// client from inbound request headers. Only the SHA-256 hex digest is kept;
// raw key material is never stored or logged.
func ClientKeyHash(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		value := strings.TrimSpace(firstHeaderValue(headers, name))
		if value == "" {
			continue
		}
		if idx := strings.Index(value, " "); idx > 0 {
			scheme := strings.ToLower(strings.TrimSpace(value[:idx]))
			if scheme == "bearer" || scheme == "apikey" || scheme == "token" {
				value = strings.TrimSpace(value[idx+1:])
			}
		}
		if value == "" {
			continue
		}
		sum := sha256.Sum256([]byte("smart-load-balancer/v1:" + value))
		return hex.EncodeToString(sum[:])
	}
	return ""
}

func firstHeaderValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// Pick chooses one candidate for the given client key hash. It returns false
// when the balancer declines to decide so the host falls back to its default
// scheduling.
func (b *Balancer) Pick(keyHash string, candidates []Candidate, cfg Config) (string, bool) {
	return b.PickWithQuota(keyHash, candidates, cfg, nil)
}

// PickWithQuota is Pick with quota-aware ordering. The resolver supplies
// per-profile quota info; a nil resolver disables quota awareness.
//
// Ordering:
//  1. Profiles the upstream marked exhausted (blocked) are excluded.
//  2. Sticky fast path: keep the client's profile while it stays eligible
//     and below the spillover threshold, so prompt caches stay warm.
//  3. Fresh selection, ranked:
//     a. user quota_priorities order (unlisted last),
//     b. quota-known profiles before unknown ones,
//     c. known: precise snapshot (used% desc) before estimated
//     (consumed tokens desc; never-used last),
//     d. unknown: host priority tier, higher first (CPA's direction),
//     e. fewest sticky owners first (avoid profiles other keys claimed),
//     f. strategy tie-break: least-connections prefers the lowest recent
//     load; round-robin cycles through the top tier in ID order.
func (b *Balancer) PickWithQuota(keyHash string, candidates []Candidate, cfg Config, resolver QuotaResolver) (string, bool) {
	cfg = cfg.WithDefaults()
	now := b.now()
	eligible := filterCandidates(candidates, cfg.Providers)
	if resolver != nil {
		kept := make([]Candidate, 0, len(eligible))
		for _, c := range eligible {
			if qi := resolver.Lookup(c.ID, c.Provider); qi.BlockedUntil.After(now) {
				continue
			}
			kept = append(kept, c)
		}
		eligible = kept
	}
	if len(eligible) == 0 {
		return "", false
	}
	if len(eligible) == 1 {
		b.record(eligible[0].ID, keyHash, cfg)
		return eligible[0].ID, true
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now, cfg)

	loads := make(map[string]int, len(eligible))
	for _, rec := range b.picks {
		loads[rec.authID]++
	}
	owners := make(map[string]int, len(eligible))
	for _, entry := range b.sticky {
		owners[entry.authID]++
	}

	// Sticky fast path.
	if cfg.Sticky && keyHash != "" {
		if entry, ok := b.sticky[keyHash]; ok && now.Sub(entry.lastUsed) <= cfg.StickyTTL() {
			if containsCandidate(eligible, entry.authID) && loads[entry.authID] < cfg.MaxInflightPerProfile {
				b.recordLocked(entry.authID, keyHash, now, cfg)
				return entry.authID, true
			}
		}
	}

	ranked := rankCandidates(eligible, cfg, resolver, keyHash, loads, owners)
	var chosen string
	if cfg.Strategy == StrategyRoundRobin {
		chosen = roundRobinTopTier(ranked, cfg, resolver, b.rrCursor)
		b.rrCursor++
	} else {
		chosen = ranked[0].ID
	}
	b.recordLocked(chosen, keyHash, now, cfg)
	return chosen, true
}

// userRank returns the position of authID in cfg.QuotaPriorities; unlisted
// profiles sort after all listed ones.
func userRank(priorities []string, authID string) int {
	for i, id := range priorities {
		if id == authID {
			return i
		}
	}
	return len(priorities) + 1<<30
}

// rankCandidates sorts candidates for fresh selection. The order is stable
// and deterministic for a fixed keyHash.
func rankCandidates(candidates []Candidate, cfg Config, resolver QuotaResolver, keyHash string, loads, owners map[string]int) []Candidate {
	infos := make(map[string]QuotaInfo, len(candidates))
	if resolver != nil {
		for _, c := range candidates {
			infos[c.ID] = resolver.Lookup(c.ID, c.Provider)
		}
	}
	ranked := make([]Candidate, len(candidates))
	copy(ranked, candidates)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, bq := ranked[i], ranked[j]
		if ra, rb := userRank(cfg.QuotaPriorities, a.ID), userRank(cfg.QuotaPriorities, bq.ID); ra != rb {
			return ra < rb
		}
		qa, qb := infos[a.ID], infos[bq.ID]
		if qa.Known != qb.Known {
			return qa.Known
		}
		if qa.Known {
			pa, pb := qa.UsedPercent != nil, qb.UsedPercent != nil
			if pa != pb {
				return pa
			}
			if pa {
				if *qa.UsedPercent != *qb.UsedPercent {
					// Fill-first: most-used (least remaining) first.
					return *qa.UsedPercent > *qb.UsedPercent
				}
			} else if qa.ConsumedTokens != qb.ConsumedTokens {
				return qa.ConsumedTokens > qb.ConsumedTokens
			}
		} else if a.Priority != bq.Priority {
			// Unknown-quota profiles follow the host priority tiers,
			// higher first, mirroring CPA's default scheduler.
			return a.Priority > bq.Priority
		}
		// Avoid profiles already claimed by other client keys.
		if owners[a.ID] != owners[bq.ID] {
			return owners[a.ID] < owners[bq.ID]
		}
		if cfg.Strategy == StrategyRoundRobin {
			return a.ID < bq.ID
		}
		if loads[a.ID] != loads[bq.ID] {
			return loads[a.ID] < loads[bq.ID]
		}
		if ta, tb := tieBreak(keyHash, a.ID), tieBreak(keyHash, bq.ID); ta != tb {
			return ta < tb
		}
		return a.ID < bq.ID
	})
	return ranked
}

// roundRobinTopTier cycles through the candidates tied with the best-ranked
// one on every key above the strategy tie-break, in ID order.
func roundRobinTopTier(ranked []Candidate, cfg Config, resolver QuotaResolver, cursor uint64) string {
	if len(ranked) == 0 {
		return ""
	}
	top := ranked[:1]
	best := ranked[0]
	bestRank, bestInfo := userRank(cfg.QuotaPriorities, best.ID), lookupInfo(resolver, best)
	for _, c := range ranked[1:] {
		if userRank(cfg.QuotaPriorities, c.ID) != bestRank {
			break
		}
		qi := lookupInfo(resolver, c)
		if !sameQuotaTier(bestInfo, qi) || (!bestInfo.Known && c.Priority != best.Priority) {
			break
		}
		top = append(top, c)
	}
	return top[cursor%uint64(len(top))].ID
}

func lookupInfo(resolver QuotaResolver, c Candidate) QuotaInfo {
	if resolver == nil {
		return QuotaInfo{}
	}
	return resolver.Lookup(c.ID, c.Provider)
}

// sameQuotaTier reports whether two quota infos tie on every ranking key
// above the strategy tie-break.
func sameQuotaTier(a, b QuotaInfo) bool {
	if a.Known != b.Known {
		return false
	}
	if !a.Known {
		return true
	}
	pa, pb := a.UsedPercent != nil, b.UsedPercent != nil
	if pa != pb {
		return false
	}
	if pa {
		return *a.UsedPercent == *b.UsedPercent
	}
	return a.ConsumedTokens == b.ConsumedTokens
}

// Reset clears all balancer state. Used by tests.
func (b *Balancer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.picks = nil
	b.sticky = make(map[string]stickyEntry)
	b.rrCursor = 0
}

// LoadSnapshot reports the current per-profile pick counts inside the window.
// Used by tests and diagnostics.
func (b *Balancer) LoadSnapshot(cfg Config) map[string]int {
	cfg = cfg.WithDefaults()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(b.now(), cfg)
	out := make(map[string]int, len(b.picks))
	for _, rec := range b.picks {
		out[rec.authID]++
	}
	return out
}

func (b *Balancer) record(authID, keyHash string, cfg Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recordLocked(authID, keyHash, b.now(), cfg)
}

func (b *Balancer) recordLocked(authID, keyHash string, now time.Time, cfg Config) {
	b.picks = append(b.picks, pickRecord{authID: authID, at: now})
	if overflow := len(b.picks) - MaxPickHistory; overflow > 0 {
		b.picks = append([]pickRecord(nil), b.picks[overflow:]...)
	}
	if cfg.Sticky && keyHash != "" {
		b.sticky[keyHash] = stickyEntry{authID: authID, lastUsed: now}
		// Opportunistically drop expired sticky entries.
		if len(b.sticky) > 4096 {
			for key, entry := range b.sticky {
				if now.Sub(entry.lastUsed) > cfg.StickyTTL() {
					delete(b.sticky, key)
				}
			}
		}
	}
}

func (b *Balancer) pruneLocked(now time.Time, cfg Config) {
	cutoff := now.Add(-cfg.Window())
	kept := b.picks[:0]
	for _, rec := range b.picks {
		if rec.at.After(cutoff) {
			kept = append(kept, rec)
		}
	}
	// Zero the tail so pruned records can be garbage collected.
	for i := len(kept); i < len(b.picks); i++ {
		b.picks[i] = pickRecord{}
	}
	b.picks = kept
	// Drop sticky assignments that have been idle past their TTL.
	for key, entry := range b.sticky {
		if now.Sub(entry.lastUsed) > cfg.StickyTTL() {
			delete(b.sticky, key)
		}
	}
}

func filterCandidates(candidates []Candidate, providers []string) []Candidate {
	if len(providers) == 0 {
		out := make([]Candidate, 0, len(candidates))
		for _, c := range candidates {
			if strings.TrimSpace(c.ID) == "" {
				continue
			}
			out = append(out, c)
		}
		return out
	}
	allowed := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		allowed[strings.ToLower(strings.TrimSpace(p))] = struct{}{}
	}
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if strings.TrimSpace(c.ID) == "" {
			continue
		}
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(c.Provider))]; ok {
			out = append(out, c)
		}
	}
	return out
}

func containsCandidate(candidates []Candidate, id string) bool {
	for _, c := range candidates {
		if c.ID == id {
			return true
		}
	}
	return false
}

func tieBreak(keyHash, candidateID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(keyHash))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(candidateID))
	return binary.LittleEndian.Uint64(h.Sum(nil)[:8])
}
