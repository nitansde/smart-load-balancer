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

// windowPeriod is the nominal length of a quota window, used to project
// the next reset when a snapshot's ResetAt has already passed (the window
// renewed but the snapshot hasn't been refreshed yet). Monthly windows
// vary between 28 and 31 days; 30 days is close enough for ordering.
func (w Window) windowPeriod() time.Duration {
	switch w.Kind {
	case WindowFiveHour:
		return 5 * time.Hour
	case WindowMonthly:
		return 30 * 24 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}

// NextReset returns the next reset time at or after now. A ResetAt that
// already passed is rolled forward by whole window periods, so the
// scheduler orders an already-renewed profile by its upcoming reset
// instead of treating it as "due now". Zero ResetAt stays zero.
func (w Window) NextReset(now time.Time) time.Time {
	if w.ResetAt.IsZero() || !w.ResetAt.Before(now) {
		return w.ResetAt
	}
	reset := w.ResetAt
	period := w.windowPeriod()
	for reset.Before(now) {
		reset = reset.Add(period)
	}
	return reset
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

// Fresh reports whether the long (weekly/monthly) window shows zero use.
// For Codex each window's countdown starts on first token use, so a
// zero-use window has no countdown running: callers may want to send one
// minimal request to start it and learn the reset time. Note this can't
// tell "never used" from "just reset" — both show 0%.
func (s Snapshot) Fresh() bool {
	if s.Long == nil || s.Long.UsedPercent == nil {
		return false
	}
	return *s.Long.UsedPercent == 0
}

// kickTolerance is how close a window's reset may sit to the full window
// length while still counting as "countdown never started". Codex rolls
// the reset forward by the whole window while a profile is idle and locks
// it in once tokens are consumed, so a reset ~one window out means idle.
const kickTolerance = 5 * time.Minute

// FiveHourFresh reports whether the five-hour window is at 100% with no
// countdown running, i.e. a borrowed real request should kick off its
// countdown the way the weekly mode does for cold profiles. True when the
// window already renewed (reset passed, effectively 100% again), or when
// it shows 0% use with a reset still about a full window out (rolling,
// never started). A 0% with a reset locked in clearly sooner means the
// countdown is running and the 0% is just rounding of tiny use.
func (s Snapshot) FiveHourFresh(now time.Time) bool {
	w := s.FiveHour
	if w == nil {
		return false
	}
	if !w.ResetAt.IsZero() && !w.ResetAt.After(now) {
		return true
	}
	if w.UsedPercent == nil || *w.UsedPercent != 0 {
		return false
	}
	if w.ResetAt.IsZero() {
		return true
	}
	return !w.ResetAt.Before(now.Add(w.windowPeriod() - kickTolerance))
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
	mu sync.RWMutex
	m  map[string]Snapshot
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{m: make(map[string]Snapshot)}
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

// MergeSnapshot overlays the non-nil windows of partial onto the stored
// snapshot, creating it when absent. It is how quota data harvested
// passively from response headers (see SnapshotFromHeaders) lands in the
// store: per-window last-write-wins, everything else is preserved.
func (s *Store) MergeSnapshot(partial Snapshot) {
	if partial.AuthID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.m[partial.AuthID]
	if !ok {
		cur = Snapshot{AuthID: partial.AuthID, Provider: partial.Provider}
	}
	if partial.FiveHour != nil {
		cur.FiveHour = partial.FiveHour
	}
	if partial.Long != nil {
		cur.Long = partial.Long
	}
	if partial.PlanType != "" {
		cur.PlanType = partial.PlanType
	}
	if !partial.FetchedAt.IsZero() {
		cur.FetchedAt = partial.FetchedAt
	}
	s.m[partial.AuthID] = cur
}

// Remove drops the snapshot for authID.
func (s *Store) Remove(authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, authID)
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
