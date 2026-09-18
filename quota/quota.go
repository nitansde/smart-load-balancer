// Package quota fetches real upstream quota snapshots for OAuth profiles
// (currently Codex) the same way the reference codex-quota-scheduler does:
// it asks the host for each auth's credential JSON via the official host
// callbacks, then queries the provider's usage endpoint with the access
// token. Snapshots feed the balancer's fill-first ordering.
//
// Raw credential material is only held in memory for the duration of a
// refresh and is never logged or persisted by this package.
package quota

import (
	"strings"
	"sync"
	"time"
)

// HasEndpoint reports whether the quota package can fetch precise upstream
// quota for the given provider key.
func HasEndpoint(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "codex")
}

// WindowKind identifies a quota window reported by the upstream usage API.
type WindowKind string

const (
	WindowFiveHour WindowKind = "five_hour"
	WindowWeekly   WindowKind = "weekly"
	WindowMonthly  WindowKind = "monthly"
)

// Window is one quota window (e.g. the 5-hour or the weekly window).
type Window struct {
	Kind WindowKind
	// UsedPercent is how much of the window is consumed (0-100), when reported.
	UsedPercent *float64
	// ResetAt is when the window resets, when reported.
	ResetAt time.Time
	// Exhausted reports the upstream flagged this window as exhausted.
	Exhausted bool
}

// Snapshot is the latest known quota state of one auth profile.
type Snapshot struct {
	AuthID    string
	Provider  string
	FiveHour  *Window
	Long      *Window // weekly or monthly, whichever the account is on
	PlanType  string
	FetchedAt time.Time
}

// Fresh reports whether the long (weekly/monthly) window has never been used.
// A fresh window's countdown starts on first use, so callers may want to
// probe it once before relying on its reset time.
func (s Snapshot) Fresh() bool {
	if s.Long == nil || s.Long.UsedPercent == nil {
		return false
	}
	return *s.Long.UsedPercent == 0
}

// LongUsedPercent returns the long window's used percent, or -1 when unknown.
// Higher means less remaining; it is the fill-first ordering key.
func (s Snapshot) LongUsedPercent() float64 {
	if s.Long == nil || s.Long.UsedPercent == nil {
		return -1
	}
	return *s.Long.UsedPercent
}

// Exhausted reports whether any known window is exhausted.
func (s Snapshot) Exhausted() bool {
	return (s.FiveHour != nil && s.FiveHour.Exhausted) ||
		(s.Long != nil && s.Long.Exhausted)
}

// EarliestReset returns the earliest known window reset time, or the zero
// time when no window reports one.
func (s Snapshot) EarliestReset() time.Time {
	var earliest time.Time
	for _, w := range []*Window{s.FiveHour, s.Long} {
		if w == nil || w.ResetAt.IsZero() {
			continue
		}
		if earliest.IsZero() || w.ResetAt.Before(earliest) {
			earliest = w.ResetAt
		}
	}
	return earliest
}

// Store holds the latest quota snapshot per auth ID. It is safe for
// concurrent use.
type Store struct {
	mu       sync.RWMutex
	m        map[string]Snapshot
	lastUsed map[string]time.Time
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{m: make(map[string]Snapshot), lastUsed: make(map[string]time.Time)}
}

// Get returns the snapshot for authID.
func (s *Store) Get(authID string) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.m[authID]
	return snap, ok
}

// Set records the snapshot for authID.
func (s *Store) Set(snap Snapshot) {
	if snap.AuthID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[snap.AuthID] = snap
}

// Remove drops the snapshot for authID.
func (s *Store) Remove(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, authID)
}

// MarkUsed records that authID just served a request. The refresher uses
// this to know whose quota numbers may have moved since the last fetch.
func (s *Store) MarkUsed(authID string) {
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed[authID] = time.Now()
}

// LastUsed returns when authID last served a request.
func (s *Store) LastUsed(authID string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.lastUsed[authID]
	return t, ok
}

// IDs lists every auth ID with a snapshot.
func (s *Store) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.m))
	for id := range s.m {
		out = append(out, id)
	}
	return out
}
