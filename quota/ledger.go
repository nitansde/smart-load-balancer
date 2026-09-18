package quota

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Estimation window lengths. The weekly Codex window starts lazily on first
// use; the five-hour window is a rolling window approximated the same way.
const (
	FiveHourWindow = 5 * time.Hour
	WeeklyWindow   = 7 * 24 * time.Hour
)

// Block reasons recorded when an upstream quota signal is observed.
const (
	BlockReasonFiveHour = "five_hour"
	BlockReasonWeekly   = "weekly"
)

// UsageObservation is one completed request reported by the host through
// usage.handle. The scheduler owns all routing decisions, so these records
// are enough to estimate quota without polling upstream.
type UsageObservation struct {
	AuthID          string
	Provider        string
	TotalTokens     int64
	Failed          bool
	StatusCode      int
	FailureBody     string
	ResponseHeaders http.Header
	ObservedAt      time.Time
}

// LedgerEntry is the estimated quota state of one auth profile.
type LedgerEntry struct {
	AuthID         string
	Provider       string
	WindowStart    time.Time
	ConsumedTokens int64
	Requests       int64
	BlockedUntil   time.Time
	BlockReason    string
	Probed         bool
	LastObserved   time.Time
}

// Blocked reports whether the profile is treated as exhausted at now.
func (e LedgerEntry) Blocked(now time.Time) bool {
	return !e.BlockedUntil.IsZero() && now.Before(e.BlockedUntil)
}

// Fresh reports whether the profile has never been observed in use.
func (e LedgerEntry) Fresh() bool {
	return !e.Probed && e.Requests == 0 && e.ConsumedTokens == 0
}

// Ledger tracks estimated quota consumption per auth ID from usage
// feedback. It is safe for concurrent use.
type Ledger struct {
	mu sync.RWMutex
	m  map[string]*LedgerEntry
}

// NewLedger returns an empty Ledger.
func NewLedger() *Ledger {
	return &Ledger{m: make(map[string]*LedgerEntry)}
}

// Observe records one completed request. Successful requests accumulate
// token consumption; failed requests carrying a quota signal mark the
// profile blocked until the estimated window reset.
func (l *Ledger) Observe(obs UsageObservation) {
	if obs.AuthID == "" {
		return
	}
	now := obs.ObservedAt
	if now.IsZero() {
		now = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[obs.AuthID]
	if !ok {
		e = &LedgerEntry{AuthID: obs.AuthID, Provider: obs.Provider, WindowStart: now}
		l.m[obs.AuthID] = e
	}
	l.rollWindowLocked(e, now)
	if !obs.Failed {
		e.ConsumedTokens += obs.TotalTokens
		e.Requests++
	}
	if reason, ok := detectExhaustion(obs); ok {
		e.BlockReason = reason
		if reason == BlockReasonWeekly {
			e.BlockedUntil = e.WindowStart.Add(WeeklyWindow)
			if !e.BlockedUntil.After(now) {
				e.BlockedUntil = now.Add(WeeklyWindow)
			}
		} else {
			e.BlockedUntil = now.Add(FiveHourWindow)
		}
	}
	e.LastObserved = now
}

// rollWindowLocked resets the weekly window once it has fully elapsed.
func (l *Ledger) rollWindowLocked(e *LedgerEntry, now time.Time) {
	if e.WindowStart.IsZero() {
		e.WindowStart = now
		return
	}
	if !now.Before(e.WindowStart.Add(WeeklyWindow)) {
		e.WindowStart = now
		e.ConsumedTokens = 0
		e.Requests = 0
		e.BlockedUntil = time.Time{}
		e.BlockReason = ""
	}
}

// Get returns a copy of the ledger entry for authID.
func (l *Ledger) Get(authID string) (LedgerEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.m[authID]
	if !ok {
		return LedgerEntry{}, false
	}
	return *e, true
}

// MarkProbed records that the fresh-window ping was already sent.
func (l *Ledger) MarkProbed(authID string) {
	if authID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[authID]
	if !ok {
		e = &LedgerEntry{AuthID: authID, WindowStart: time.Now()}
		l.m[authID] = e
	}
	e.Probed = true
}

// detectExhaustion inspects a failed request for an upstream quota signal.
// It returns which window the signal belongs to. When the signal does not
// name a window — or there is no failure text at all to classify — it
// conservatively reports the five-hour window so the profile recovers
// sooner rather than later.
func detectExhaustion(obs UsageObservation) (string, bool) {
	if !obs.Failed || obs.StatusCode != 429 {
		return "", false
	}
	var sb strings.Builder
	sb.WriteString(strings.ToLower(obs.FailureBody))
	sb.WriteByte(' ')
	for key, values := range obs.ResponseHeaders {
		sb.WriteString(strings.ToLower(key))
		sb.WriteByte(' ')
		for _, v := range values {
			sb.WriteString(strings.ToLower(v))
			sb.WriteByte(' ')
		}
	}
	text := strings.TrimSpace(sb.String())
	if strings.Contains(text, "week") || strings.Contains(text, "month") {
		return BlockReasonWeekly, true
	}
	if looksLikeQuotaSignal(text) || text == "" {
		return BlockReasonFiveHour, true
	}
	if looksLikeTransientLimit(text) {
		return "", false
	}
	return BlockReasonFiveHour, true
}

func looksLikeQuotaSignal(text string) bool {
	for _, kw := range []string{
		"quota", "rate limit", "ratelimit", "rate_limit",
		"too many requests", "exceed",
	} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// looksLikeTransientLimit identifies 429s that are clearly about request
// concurrency rather than quota, so a busy profile is not sidelined for
// five hours on a burst.
func looksLikeTransientLimit(text string) bool {
	return strings.Contains(text, "concurrent")
}
