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
	cfg = cfg.WithDefaults()
	eligible := filterCandidates(candidates, cfg.Providers)
	if len(eligible) == 0 {
		return "", false
	}
	if len(eligible) == 1 {
		b.record(eligible[0].ID, keyHash, cfg)
		return eligible[0].ID, true
	}

	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now, cfg)

	loads := make(map[string]int, len(eligible))
	for _, rec := range b.picks {
		loads[rec.authID]++
	}

	// Sticky fast path: keep the client's profile while it stays eligible and
	// below the spillover threshold, so prompt caches stay warm.
	if cfg.Sticky && keyHash != "" {
		if entry, ok := b.sticky[keyHash]; ok && now.Sub(entry.lastUsed) <= cfg.StickyTTL() {
			if containsCandidate(eligible, entry.authID) && loads[entry.authID] < cfg.MaxInflightPerProfile {
				b.recordLocked(entry.authID, keyHash, now, cfg)
				return entry.authID, true
			}
		}
	}

	var chosen string
	switch cfg.Strategy {
	case StrategyRoundRobin:
		chosen = roundRobinPick(eligible, b.rrCursor)
		b.rrCursor++
	default:
		chosen = leastConnectionsPick(eligible, loads, keyHash)
	}
	b.recordLocked(chosen, keyHash, now, cfg)
	return chosen, true
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

// leastConnectionsPick returns the candidate with the fewest recent picks.
// Ties are broken by a deterministic hash of (keyHash, candidateID) so that
// different client keys spread across profiles instead of colliding on the
// same one, while the same key stays stable when nothing else changed.
func leastConnectionsPick(candidates []Candidate, loads map[string]int, keyHash string) string {
	best := candidates[0]
	bestLoad := loads[best.ID]
	bestTie := tieBreak(keyHash, best.ID)
	for _, c := range candidates[1:] {
		load := loads[c.ID]
		tie := tieBreak(keyHash, c.ID)
		if load < bestLoad || (load == bestLoad && tie < bestTie) {
			best, bestLoad, bestTie = c, load, tie
		}
	}
	return best.ID
}

func roundRobinPick(candidates []Candidate, cursor uint64) string {
	sorted := make([]Candidate, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted[cursor%uint64(len(sorted))].ID
}

func tieBreak(keyHash, candidateID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(keyHash))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(candidateID))
	return binary.LittleEndian.Uint64(h.Sum(nil)[:8])
}
