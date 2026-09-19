package quota

import (
	"encoding/json"
	"math/rand"
	"strings"
	"sync"
	"time"
)

const (
	// codexUsageEndpoint reports five-hour/weekly/monthly quota windows.
	codexUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"
	// codexProbeEndpoint accepts a minimal request used to kick off a fresh
	// (never-used) weekly window's countdown.
	codexProbeEndpoint = "https://chatgpt.com/backend-api/codex/responses/compact"
	codexProbeModel    = "gpt-5.4-mini"
	codexProbePayload  = `{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	codexUserAgent     = "codex_cli_rs/0.76.0"

	// defaultMaxStale is the backstop refresh age: even a profile nobody
	// used gets re-checked this often, catching quota consumed outside CPA.
	defaultMaxStale = 6 * time.Hour

	// defaultSpreadWindow is the window over which one background
	// calibration cycle spreads its per-auth fetches. With N profiles due,
	// fetches start spread/N apart, so the upstream endpoint is never hit
	// by all profiles at once.
	defaultSpreadWindow = 6 * time.Hour
	// onDemandMinInterval is the minimum spacing between on-demand
	// (reset-expiry) quota fetches. When many profiles' windows expire
	// together, their re-fetches queue up this far apart instead of
	// bursting the upstream endpoint.
	onDemandMinInterval = 10 * time.Minute
	// failedCooldown is how long a profile whose quota fetch failed is
	// left alone before any path tries it again.
	failedCooldown = 5 * time.Hour
)

// usageEndpoints maps a provider to its quota endpoint. Providers without an
// entry are skipped: we only query endpoints we understand.
var usageEndpoints = map[string]string{
	"codex": codexUsageEndpoint,
}

// AuthEntry describes one host auth record.
type AuthEntry struct {
	ID        string
	AuthIndex string
	Provider  string
}

// HTTPRequest is a plain outbound HTTP request executed by the host.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// HTTPResponse is the host-executed HTTP response.
type HTTPResponse struct {
	StatusCode int
	Body       []byte
}

// HostClient abstracts the host callbacks used for quota refresh so the
// refresher stays testable without cgo.
type HostClient interface {
	ListAuths() ([]AuthEntry, error)
	GetAuthJSON(authIndex string) (json.RawMessage, error)
	DoHTTP(req HTTPRequest) (HTTPResponse, error)
}

// Config controls the refresher. It is read fresh on every cycle so
// plugin.reconfigure takes effect without a restart.
type Config struct {
	Enabled   bool
	Providers []string
	Interval  time.Duration
	// ProbeFresh sends one minimal "hi" request when a refresh shows the
	// long window's reset interval at about the full window length (~7d /
	// 30d). Codex keeps reset_at rolling one full window out while idle;
	// only token use locks it in. The "hi" starts the new window's
	// countdown, making its reset time real for reset-soonest ordering.
	ProbeFresh bool
	// MaxStale caps how old a snapshot may get before it is re-fetched even
	// when nothing else changed. Zero means defaultMaxStale.
	MaxStale time.Duration
}

// Refresher periodically pulls upstream quota snapshots into a Store.
type Refresher struct {
	client HostClient
	store  *Store
	// ledger is the usage-feedback ledger, the authority on whether a
	// profile was ever used: upstream used_percent rounds tiny usage to
	// 0%, so it alone cannot prove "never used". May be nil in tests,
	// which then fall back to the snapshot-only check.
	ledger *Ledger
	config func() Config

	mu       sync.Mutex
	running  bool
	stop     chan struct{}
	wg       sync.WaitGroup
	onDemand map[string]time.Time
	// nextOnDemand is the earliest time an on-demand fetch may start; it
	// enforces the minimum spacing between on-demand fetches across
	// profiles.
	nextOnDemand time.Time
	// failedAt records the last fetch failure per auth. A failed auth is
	// not re-fetched until failedCooldown has passed.
	failedAt map[string]time.Time
	// spread staggers one calibration cycle's fetches; fetchGap spaces
	// on-demand fetches. Tests override the const defaults.
	spread   time.Duration
	fetchGap time.Duration
}

// NewRefresher returns a Refresher that is not yet running.
func NewRefresher(client HostClient, store *Store, ledger *Ledger, config func() Config) *Refresher {
	return &Refresher{
		client:   client,
		store:    store,
		ledger:   ledger,
		config:   config,
		failedAt: make(map[string]time.Time),
		spread:   defaultSpreadWindow,
		fetchGap: onDemandMinInterval,
	}
}

// Start begins background refresh. It is idempotent.
func (r *Refresher) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return
	}
	r.running = true
	r.stop = make(chan struct{})
	r.wg.Add(1)
	go r.loop()
}

// Stop halts background refresh. It is idempotent.
func (r *Refresher) Stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	close(r.stop)
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Refresher) loop() {
	defer r.wg.Done()
	cfg := r.config()
	if cfg.Interval <= 0 {
		// Background calibration disabled: precise quota is fetched only
		// on demand (manual refresh through the quota provider). The
		// usage-feedback ledger keeps working regardless.
		return
	}
	r.RefreshOnce()
	for {
		interval := r.config().Interval
		if interval <= 0 {
			return
		}
		// Jitter the wait so multiple plugin instances do not burst together.
		wait := interval + time.Duration(rand.Int63n(int64(interval)/4+1))
		select {
		case <-r.stop:
			return
		case <-time.After(wait):
			r.RefreshOnce()
		}
	}
}

// RefreshOnce pulls a fresh quota snapshot for every eligible auth whose
// quota could have changed. Fetches are staggered: with N profiles due,
// they start spread/N apart inside the spread window, so one calibration
// cycle never bursts the upstream endpoint. Profiles whose last fetch
// failed, or with a recently scheduled on-demand fetch, are skipped.
func (r *Refresher) RefreshOnce() {
	cfg := r.config()
	if !cfg.Enabled {
		return
	}
	auths, err := r.client.ListAuths()
	if err != nil {
		return
	}
	eligible := filterAuths(auths, cfg.Providers)
	now := time.Now()
	var due []AuthEntry
	for _, auth := range eligible {
		if r.needsRefresh(auth, cfg, now) &&
			!r.inFailureCooldown(auth.ID, now) &&
			!r.onDemandPending(auth.ID, now) {
			due = append(due, auth)
		}
	}
	// Drop snapshots for auths that no longer exist.
	seen := make(map[string]bool, len(eligible))
	for _, a := range eligible {
		seen[a.ID] = true
	}
	for _, id := range r.store.IDs() {
		if !seen[id] {
			r.store.Remove(id)
		}
	}
	var gap time.Duration
	if n := len(due); n > 0 {
		gap = r.spreadOrDefault() / time.Duration(n)
	}
	for i, auth := range due {
		if i > 0 {
			select {
			case <-r.stop:
				return
			case <-time.After(gap):
			}
		}
		if r.inFailureCooldown(auth.ID, time.Now()) {
			continue
		}
		r.refreshOne(auth, cfg)
	}
}

// RefreshAuthNow re-fetches one auth's quota snapshot in the background.
// It is meant for snapshots whose window reset time has passed: the stored
// numbers are stale, and the next pick should see fresh ones instead of
// waiting for the next background cycle.
//
// On-demand fetches never burst: every fetch starts at least fetchGap
// after the previous on-demand fetch, no matter how many profiles expired
// at once, and each auth is single-flighted within the gap. A failed fetch
// cools the auth down for failedCooldown before any path retries it.
// Works even when background calibration is disabled (on-demand only).
func (r *Refresher) RefreshAuthNow(authID string) {
	if r == nil || authID == "" {
		return
	}
	cfg := r.config()
	if !cfg.Enabled {
		return
	}
	gap := r.gapOrDefault()
	now := time.Now()
	r.mu.Lock()
	if r.onDemand == nil {
		r.onDemand = make(map[string]time.Time)
	}
	if last, ok := r.onDemand[authID]; ok && now.Sub(last) < gap {
		r.mu.Unlock()
		return
	}
	at := now
	if r.nextOnDemand.After(at) {
		at = r.nextOnDemand
	}
	r.nextOnDemand = at.Add(gap)
	r.onDemand[authID] = at
	r.mu.Unlock()

	delay := at.Sub(now)
	go func() {
		select {
		case <-r.stop:
			return
		case <-time.After(delay):
		}
		if r.inFailureCooldown(authID, time.Now()) {
			return
		}
		auths, err := r.client.ListAuths()
		if err != nil {
			return
		}
		for _, a := range auths {
			if a.ID == authID {
				r.refreshOne(a, cfg)
				return
			}
		}
		// Auth vanished: drop its snapshot.
		r.store.Remove(authID)
	}()
}

// spreadOrDefault returns the configured stagger window, defaulting to
// defaultSpreadWindow.
func (r *Refresher) spreadOrDefault() time.Duration {
	if r.spread > 0 {
		return r.spread
	}
	return defaultSpreadWindow
}

// gapOrDefault returns the configured on-demand spacing, defaulting to
// onDemandMinInterval.
func (r *Refresher) gapOrDefault() time.Duration {
	if r.fetchGap > 0 {
		return r.fetchGap
	}
	return onDemandMinInterval
}

// inFailureCooldown reports whether authID's last fetch failed recently
// enough that no path should retry it yet.
func (r *Refresher) inFailureCooldown(authID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.failedAt[authID]
	return ok && now.Sub(last) < failedCooldown
}

// onDemandPending reports whether an on-demand fetch was scheduled for
// authID recently enough that the periodic cycle should leave it alone.
func (r *Refresher) onDemandPending(authID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.onDemand[authID]
	return ok && now.Sub(last) < r.gapOrDefault()
}

func (r *Refresher) noteFailure(authID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failedAt == nil {
		r.failedAt = make(map[string]time.Time)
	}
	r.failedAt[authID] = time.Now()
}

func (r *Refresher) clearFailure(authID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failedAt, authID)
}

// needsRefresh reports whether auth's quota could have changed since its
// last fetch. Quota numbers only move when the profile is used, when a
// window resets, or when it is consumed outside CPA — so idle profiles are
// left alone instead of being polled every cycle.
func (r *Refresher) needsRefresh(auth AuthEntry, cfg Config, now time.Time) bool {
	snap, ok := r.store.Get(auth.ID)
	if !ok {
		return true // never fetched
	}
	// Requests we routed to this profile since the snapshot moved its numbers.
	if lastUsed, ok := r.store.LastUsed(auth.ID); ok && lastUsed.After(snap.FetchedAt) {
		return true
	}
	// A weekly window whose reset time has passed may hold fresh numbers.
	// (The five-hour window is estimated locally from usage feedback: a
	// wrong estimate surfaces as a failed request, which the ledger turns
	// into a five-hour block. It never triggers a re-fetch on its own.)
	if w := snap.Long; w != nil && !w.ResetAt.IsZero() && !w.ResetAt.After(now) {
		return true
	}
	// Backstop: catch quota consumed outside CPA.
	maxStale := cfg.MaxStale
	if maxStale <= 0 {
		maxStale = defaultMaxStale
	}
	return now.Sub(snap.FetchedAt) >= maxStale
}

func (r *Refresher) refreshOne(auth AuthEntry, cfg Config) {
	snap, err := FetchSnapshot(r.client, auth)
	if err != nil {
		// A failed fetch cools the auth down for five hours before any
		// path retries it, instead of hammering a broken endpoint.
		r.noteFailure(auth.ID)
		return
	}
	r.clearFailure(auth.ID)
	r.store.Set(snap)

	// Kick off the long window's countdown when it isn't running. Codex
	// keeps reset_at about one full window in the future while the window
	// is idle (it rolls forward on every fetch); only token use locks it
	// in, after which the interval shrinks. So a refresh showing an
	// interval of ~7d/30d means the new window hasn't started: one minimal
	// "hi" starts it, making the reset time real for reset-soonest
	// ordering. This is per window, not once ever.
	if cfg.ProbeFresh && r.shouldKick(auth.ID, snap) {
		if creds, err := CredentialsForAuth(r.client, auth); err == nil {
			ProbeFreshWindow(r.client.DoHTTP, creds)
			if r.ledger != nil {
				r.ledger.MarkProbed(auth.ID)
			}
		}
	}
}

// kickTolerance bounds how far the reset interval may fall short of the
// full window length while still counting as "countdown not started".
// An idle window's reset_at rolls forward on every fetch, so the measured
// interval jitters by fetch latency; five minutes covers that without
// misjudging a countdown that has been running for a while.
const kickTolerance = 5 * time.Minute

// shouldKick reports whether the long window's countdown needs starting.
// Codex keeps reset_at about one full window in the future while the
// window is idle — it keeps rolling forward on every fetch. Only token
// use locks it in, after which the interval (reset_at - now) shrinks. So
// the countdown is "not running" exactly when the interval is about the
// full window length; used_percent can't tell, since tiny usage rounds to
// 0%.
func (r *Refresher) shouldKick(authID string, snap Snapshot) bool {
	w := snap.Long
	if w == nil {
		return false
	}
	if w.ResetAt.IsZero() {
		return true // no countdown info at all; kick to start one
	}
	if w.ResetAt.Sub(time.Now()) < w.windowPeriod()-kickTolerance {
		return false // interval is shrinking: countdown already running
	}
	if r.ledger != nil {
		if e, ok := r.ledger.Get(authID); ok && e.LastObserved.After(snap.FetchedAt) {
			return false // stale snapshot: that usage started a countdown
		}
	}
	return true
}

func filterAuths(auths []AuthEntry, providers []string) []AuthEntry {
	allowed := make(map[string]bool, len(providers))
	for _, p := range providers {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			allowed[p] = true
		}
	}
	var out []AuthEntry
	for _, a := range auths {
		provider := strings.ToLower(strings.TrimSpace(a.Provider))
		if _, ok := usageEndpoints[provider]; !ok {
			continue
		}
		if len(allowed) > 0 && !allowed[provider] {
			continue
		}
		if strings.TrimSpace(a.ID) == "" || strings.TrimSpace(a.AuthIndex) == "" {
			continue
		}
		out = append(out, AuthEntry{ID: a.ID, AuthIndex: a.AuthIndex, Provider: provider})
	}
	return out
}
