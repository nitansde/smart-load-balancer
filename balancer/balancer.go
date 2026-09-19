package balancer

import (
	"crypto/sha256"
	"encoding/hex"
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
	// LongResetAt is when the long-window quota resets, when known from a
	// precise snapshot. The long window is weekly or monthly, whichever
	// the account is on; both are treated identically. Reset-soonest
	// profiles are preferred: quota that renews soon should be spent first.
	LongResetAt time.Time
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

// historyEntry remembers a client key's most recently used profile,
// backing the last-used preference after the sticky TTL expires.
type historyEntry struct {
	authID string
	at     time.Time
}

// historyTTL bounds how long a client's last-used profile is remembered
// for the preference ranking. maxHistoryEntries caps the map; beyond it
// the oldest entries are evicted.
const (
	historyTTL       = 7 * 24 * time.Hour
	maxHistoryEntries = 65536
)

// Balancer spreads picks across candidates. It is safe for concurrent use;
// the host may invoke scheduler.pick from many goroutines at once.
type Balancer struct {
	mu      sync.Mutex
	now     func() time.Time
	picks   []pickRecord
	sticky  map[string]stickyEntry
	history map[string]historyEntry
}

// New returns a Balancer using the real clock.
func New() *Balancer {
	return &Balancer{now: time.Now, sticky: make(map[string]stickyEntry), history: make(map[string]historyEntry)}
}

// newWithClock is used by tests to control time.
func newWithClock(now func() time.Time) *Balancer {
	return &Balancer{now: now, sticky: make(map[string]stickyEntry), history: make(map[string]historyEntry)}
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
//     b. profiles claimed by other client keys sort after every unclaimed
//     profile (no-conflict guarantee; only when all candidates are
//     claimed does the fewest-owners key in (g) decide),
//     c. quota-known profiles before unknown ones,
//     d. known: long-window reset soonest first (weekly or monthly,
//     whichever the account is on; resets within an hour tie),
//     e. same reset tier: fill-first, precise used% desc, else ledger
//     consumed tokens desc, never-used last,
//     f. unknown: host priority tier, higher first (CPA's direction),
//     g. fewest other-key sticky owners first,
//     h. least recent load, then natural ID order, first wins
//     (fill-first).
//     The strategy setting is accepted for compatibility but ignored:
//     tiers are strict (the best tier always wins); rotation only happens
//     among candidates tied on every ranking key.
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
	// owners counts sticky claims by *other* client keys. Fresh selection
	// guarantees no conflict while any unclaimed profile is available: a
	// profile claimed by another key is only chosen when every candidate
	// is claimed, and then the fewest-claimed one wins.
	owners := make(map[string]int, len(eligible))
	for kh, entry := range b.sticky {
		if kh == keyHash {
			continue
		}
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

	rs := newRankState(eligible, cfg, resolver, keyHash, loads, owners, b.history)
	ranked := rs.sorted(eligible)
	// Fill-first on ties: take the head of the ranking. The strategy
	// setting stays ignored.
	chosen := ranked[0].ID
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

// rankState bundles the inputs to candidate ranking.
type rankState struct {
	cfg     Config
	infos   map[string]QuotaInfo
	loads   map[string]int
	owners  map[string]int
	keyHash string
	history map[string]historyEntry
	now     time.Time
}

func newRankState(candidates []Candidate, cfg Config, resolver QuotaResolver, keyHash string, loads, owners map[string]int, history map[string]historyEntry) *rankState {
	infos := make(map[string]QuotaInfo, len(candidates))
	if resolver != nil {
		for _, c := range candidates {
			infos[c.ID] = resolver.Lookup(c.ID, c.Provider)
		}
	}
	return &rankState{cfg: cfg, infos: infos, loads: loads, owners: owners, keyHash: keyHash, history: history, now: time.Now()}
}

// compare orders two candidates for fresh selection and returns 0 when
// they tie on every ranking key. The order is deterministic.
func (s *rankState) compare(a, b Candidate) int {
	if ra, rb := userRank(s.cfg.QuotaPriorities, a.ID), userRank(s.cfg.QuotaPriorities, b.ID); ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	// No-conflict guarantee: a profile claimed by another client key
	// sorts after every unclaimed profile. The owners key further
	// below only decides among claimed profiles when no unclaimed
	// candidate exists at all.
	if ca, cb := s.owners[a.ID] > 0, s.owners[b.ID] > 0; ca != cb {
		if cb {
			return -1
		}
		return 1
	}
	qa, qb := s.infos[a.ID], s.infos[b.ID]
	if qa.Known != qb.Known {
		if qa.Known {
			return -1
		}
		return 1
	}
	if qa.Known {
		// Reset-soonest first: spend quota that renews soon before
		// quota with a distant reset. Reset moments within
		// resetTieWindow count as the same moment and fall through
		// to the fill-first keys below.
		if longResetLess(qa.LongResetAt, qb.LongResetAt, s.now) {
			return -1
		}
		if longResetLess(qb.LongResetAt, qa.LongResetAt, s.now) {
			return 1
		}
		pa, pb := qa.UsedPercent != nil, qb.UsedPercent != nil
		if pa != pb {
			if pa {
				return -1
			}
			return 1
		}
		if pa {
			if *qa.UsedPercent != *qb.UsedPercent {
				// Fill-first: most-used (least remaining) first.
				if *qa.UsedPercent > *qb.UsedPercent {
					return -1
				}
				return 1
			}
		} else if qa.ConsumedTokens != qb.ConsumedTokens {
			if qa.ConsumedTokens > qb.ConsumedTokens {
				return -1
			}
			return 1
		}
	} else if a.Priority != b.Priority {
		// Unknown-quota profiles follow the host priority tiers,
		// higher first, mirroring CPA's default scheduler.
		if a.Priority > b.Priority {
			return -1
		}
		return 1
	}
	// Only reached when every candidate is claimed by other keys:
	// prefer the fewest-claimed profile.
	if s.owners[a.ID] != s.owners[b.ID] {
		if s.owners[a.ID] < s.owners[b.ID] {
			return -1
		}
		return 1
	}
	if s.loads[a.ID] != s.loads[b.ID] {
		if s.loads[a.ID] < s.loads[b.ID] {
			return -1
		}
		return 1
	}
	// Prefer the client's most recently used profile. Soft preference
	// only: it never overrides the no-conflict guarantee, quota
	// ordering, or load above.
	if ha, hb := s.history[s.keyHash].authID == a.ID, s.history[s.keyHash].authID == b.ID; ha != hb {
		if ha {
			return -1
		}
		return 1
	}
	// Final key: candidate ID in natural order (numeric runs compared by
	// value, so "profile-2" sorts before "profile-10"), mirroring CPA's
	// default scheduler which orders each priority tier by auth ID.
	// CPA's IDs are UUIDs, for which natural order matches plain string
	// comparison exactly.
	if a.ID != b.ID {
		if naturalIDLess(a.ID, b.ID) {
			return -1
		}
		return 1
	}
	return 0
}

// sorted returns candidates ordered for fresh selection.
func (s *rankState) sorted(candidates []Candidate) []Candidate {
	ranked := make([]Candidate, len(candidates))
	copy(ranked, candidates)
	sort.SliceStable(ranked, func(i, j int) bool { return s.compare(ranked[i], ranked[j]) < 0 })
	return ranked
}


// resetTieWindow is the tolerance within which two long-window reset
// moments count as the same reset: the scheduler treats them as one tier
// and falls through to the fill-first keys.
const resetTieWindow = time.Hour

// longResetLess reports whether profile a's long-window quota resets
// meaningfully sooner than b's. The long window is weekly or monthly,
// whichever the account is on; both sort by reset time identically.
// Profiles with a known reset sort before profiles without one; a reset
// already in the past is treated as now
// (its quota just renewed). Reset moments within resetTieWindow of each
// other tie, so the caller falls through to the next ranking key.
func longResetLess(a, b time.Time, now time.Time) bool {
	az, bz := a.IsZero(), b.IsZero()
	if az != bz {
		return bz
	}
	if az {
		return false
	}
	// Defensive: the resolver normally projects past resets forward to
	// the next window already; clamp here so a stale time can't win
	// outright.
	if a.Before(now) {
		a = now
	}
	if b.Before(now) {
		b = now
	}
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	if d <= resetTieWindow {
		return false
	}
	return a.Before(b)
}

// longResetTie reports whether two long-window reset moments count as the
// same reset tier for grouping (e.g. round-robin top-tier selection).
func longResetTie(a, b time.Time, now time.Time) bool {
	return !longResetLess(a, b, now) && !longResetLess(b, a, now)
}

// Reset clears all balancer state. Used by tests.
func (b *Balancer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.picks = nil
	b.sticky = make(map[string]stickyEntry)
	b.history = make(map[string]historyEntry)
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
	if keyHash != "" {
		b.history[keyHash] = historyEntry{authID: authID, at: now}
		if len(b.history) > maxHistoryEntries {
			b.evictOldHistoryLocked()
		}
	}
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
	// Forget last-used profiles remembered too long ago.
	for key, entry := range b.history {
		if now.Sub(entry.at) > historyTTL {
			delete(b.history, key)
		}
	}
}

// evictOldHistoryLocked drops the oldest ~10% of history entries when the
// map exceeds maxHistoryEntries. Callers must hold b.mu.
func (b *Balancer) evictOldHistoryLocked() {
	type kv struct {
		key string
		at  time.Time
	}
	all := make([]kv, 0, len(b.history))
	for key, entry := range b.history {
		all = append(all, kv{key, entry.at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	drop := len(all) - maxHistoryEntries*9/10
	for i := 0; i < drop; i++ {
		delete(b.history, all[i].key)
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

// naturalIDLess compares IDs with embedded numeric runs compared by
// value, so "profile-2" sorts before "profile-10". Non-numeric parts
// compare byte-wise. For fixed-format IDs such as CPA's UUIDs this
// matches plain string comparison exactly.
func naturalIDLess(a, b string) bool {
	ia, ib := 0, 0
	for ia < len(a) && ib < len(b) {
		ca, cb := a[ia], b[ib]
		da, db := ca >= '0' && ca <= '9', cb >= '0' && cb <= '9'
		if da && db {
			ja, jb := ia, ib
			for ja < len(a) && a[ja] >= '0' && a[ja] <= '9' {
				ja++
			}
			for jb < len(b) && b[jb] >= '0' && b[jb] <= '9' {
				jb++
			}
			na, nb := strings.TrimLeft(a[ia:ja], "0"), strings.TrimLeft(b[ib:jb], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			if ja-ia != jb-ib {
				return ja-ia < jb-ib
			}
			ia, ib = ja, jb
			continue
		}
		if ca != cb {
			return ca < cb
		}
		ia++
		ib++
	}
	return len(a) < len(b)
}
