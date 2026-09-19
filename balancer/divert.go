package balancer

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Calibration diversion borrows one real user request to calibrate a
// stale or cold account, instead of synthesizing probe requests. The
// plugin never creates traffic of its own: when an account needs a fresh
// quota snapshot, the next suitable real request is routed to it for one
// shot, and the response headers (harvested passively by usage.handle)
// supply the numbers. No suitable traffic means no calibration — stale
// data is always preferable to a synthetic request.

const (
	// keyTokenWindow bounds per-key input-token history: only the most
	// recent 100 requests feed a key's mean, keeping memory flat.
	keyTokenWindow = 100
	// globalTokenWindow bounds the all-keys history feeding the
	// small-request threshold.
	globalTokenWindow = 50
	// minGlobalSamples is the minimum global history needed before the
	// small-key gate may pass. Below this, diversion waits for the
	// timeout fallback instead of guessing.
	minGlobalSamples = 10
	// defaultCalibrationTimeout is how long a calibration mark may wait
	// for a small-key request before any key's request may be borrowed.
	defaultCalibrationTimeout = 6 * time.Hour
	// divertAttemptCooldown spaces repeated borrow attempts for one
	// account: a borrowed request that taught us nothing (non-quota
	// failure) doesn't immediately cost the user another one.
	divertAttemptCooldown = 10 * time.Minute
)

// tokenWindow is a bounded ring of token counts with a running sum, so
// the mean stays O(1) no matter the window size.
type tokenWindow struct {
	buf  []int64
	sum  int64
	next int
}

func newTokenWindow(size int) *tokenWindow {
	return &tokenWindow{buf: make([]int64, 0, size)}
}

func (w *tokenWindow) add(n int64) {
	if len(w.buf) < cap(w.buf) {
		w.buf = append(w.buf, n)
		w.sum += n
		return
	}
	w.sum -= w.buf[w.next]
	w.buf[w.next] = n
	w.sum += n
	w.next = (w.next + 1) % cap(w.buf)
}

func (w *tokenWindow) mean() (float64, bool) {
	if len(w.buf) == 0 {
		return 0, false
	}
	return float64(w.sum) / float64(len(w.buf)), true
}

// percentile10 returns the 10th percentile of the window's samples
// (the largest value still in the bottom tenth).
func (w *tokenWindow) percentile10() (int64, bool) {
	n := len(w.buf)
	if n == 0 {
		return 0, false
	}
	cp := make([]int64, n)
	copy(cp, w.buf)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(math.Ceil(0.1*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	return cp[idx], true
}

// KeyStats tracks input-token sizes per client key and globally. Feed it
// from usage.handle; read it from the pick path.
type KeyStats struct {
	mu     sync.Mutex
	keys   map[string]*tokenWindow
	global *tokenWindow
}

// NewKeyStats returns empty statistics.
func NewKeyStats() *KeyStats {
	return &KeyStats{
		keys:   make(map[string]*tokenWindow),
		global: newTokenWindow(globalTokenWindow),
	}
}

// Observe records one completed request's input tokens. Non-positive
// counts (missing data) are ignored. Safe for concurrent use.
func (s *KeyStats) Observe(keyHash string, inputTokens int64) {
	if inputTokens <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.global.add(inputTokens)
	if keyHash == "" {
		return
	}
	w, ok := s.keys[keyHash]
	if !ok {
		w = newTokenWindow(keyTokenWindow)
		s.keys[keyHash] = w
	}
	w.add(inputTokens)
}

// MeanFor returns the key's mean input tokens over its recent window.
func (s *KeyStats) MeanFor(keyHash string) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.keys[keyHash]
	if !ok {
		return 0, false
	}
	return w.mean()
}

// SmallThreshold returns the global 10th-percentile input-token count:
// a key whose mean is at or below this counts as "small". It reports
// false until enough history exists to make the percentile meaningful.
func (s *KeyStats) SmallThreshold() (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.global.buf) < minGlobalSamples {
		return 0, false
	}
	return s.global.percentile10()
}

// calibrationMark tracks one account waiting for a borrowed request.
type calibrationMark struct {
	markedAt    time.Time
	lastAttempt time.Time
}

// PendingCalibrations is the set of accounts whose quota snapshot needs
// refreshing via a borrowed real request. Marks are idempotent and
// cleared as soon as any real request to the account teaches us
// something (passive headers on success, quota classification on a
// quota failure).
type PendingCalibrations struct {
	mu    sync.Mutex
	marks map[string]*calibrationMark
}

// NewPendingCalibrations returns an empty mark set.
func NewPendingCalibrations() *PendingCalibrations {
	return &PendingCalibrations{marks: make(map[string]*calibrationMark)}
}

// Mark flags authID for calibration. Re-marking keeps the earliest mark
// time so the timeout fallback measures from first need, not last.
func (p *PendingCalibrations) Mark(authID string) {
	if authID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.marks[authID]; ok {
		if m.markedAt.IsZero() {
			m.markedAt = time.Now()
		}
		return
	}
	p.marks[authID] = &calibrationMark{markedAt: time.Now()}
}

// Clear drops authID's mark.
func (p *PendingCalibrations) Clear(authID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.marks, authID)
}

// Attempted records that a request was just borrowed for authID, starting
// its retry cooldown.
func (p *PendingCalibrations) Attempted(authID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.marks[authID]; ok {
		m.lastAttempt = time.Now()
	}
}

// Len returns the number of pending marks.
func (p *PendingCalibrations) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.marks)
}

// oldestTarget returns the longest-waiting marked auth that is among the
// eligible candidates and past its attempt cooldown.
func (p *PendingCalibrations) oldestTarget(eligible []Candidate, now time.Time) (authID string, markedAt time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	inEligible := make(map[string]bool, len(eligible))
	for _, c := range eligible {
		inEligible[c.ID] = true
	}
	for id, m := range p.marks {
		if !inEligible[id] {
			continue
		}
		if !m.lastAttempt.IsZero() && now.Sub(m.lastAttempt) < divertAttemptCooldown {
			continue
		}
		if !ok || m.markedAt.Before(markedAt) {
			authID, markedAt, ok = id, m.markedAt, true
		}
	}
	return authID, markedAt, ok
}

// DivertState bundles everything calibration diversion needs.
type DivertState struct {
	Stats   *KeyStats
	Pending *PendingCalibrations
	// Timeout is how long a mark waits for a small-key request before any
	// key's request may be borrowed. Zero means defaultCalibrationTimeout.
	Timeout time.Duration
}

// NewDivertState returns diversion state with default settings.
func NewDivertState() *DivertState {
	return &DivertState{
		Stats:   NewKeyStats(),
		Pending: NewPendingCalibrations(),
		Timeout: defaultCalibrationTimeout,
	}
}

func (d *DivertState) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return defaultCalibrationTimeout
}

// KeyHashForValue hashes a raw client key value (scheme prefix stripped)
// with the same domain separator as ClientKeyHash, so usage.handle can
// attribute a completed request to the same key the pick path saw.
func KeyHashForValue(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.Index(value, " "); idx > 0 {
		if scheme := strings.ToLower(strings.TrimSpace(value[:idx])); scheme == "bearer" || scheme == "apikey" || scheme == "token" {
			value = strings.TrimSpace(value[idx+1:])
		}
	}
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("smart-load-balancer/v1:" + value))
	return hex.EncodeToString(sum[:])
}

// divertTarget decides whether the current pick should be borrowed to
// calibrate a marked account. It returns the target auth ID when:
//
//   - some marked account is among the eligible candidates and past its
//     attempt cooldown, and
//   - either the requesting key's mean input tokens are at/below the
//     global 10th percentile (small request, cheap to borrow), or the
//     oldest mark has waited longer than the timeout (no small-key
//     traffic showed up; borrow whatever comes rather than starve).
func (b *Balancer) divertTarget(keyHash string, eligible []Candidate, now time.Time) (string, bool) {
	b.mu.Lock()
	d := b.divert
	b.mu.Unlock()
	if d == nil || d.Stats == nil || d.Pending == nil {
		return "", false
	}
	target, markedAt, ok := d.Pending.oldestTarget(eligible, now)
	if !ok {
		return "", false
	}
	if mean, okMean := d.Stats.MeanFor(keyHash); okMean {
		if threshold, okThreshold := d.Stats.SmallThreshold(); okThreshold && mean <= float64(threshold) {
			return target, true
		}
	}
	if now.Sub(markedAt) >= d.timeout() {
		return target, true
	}
	return "", false
}
