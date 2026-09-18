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
	codexProbePayload  = `{"model":"gpt-5.4-mini","instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}`
	codexUserAgent     = "codex_cli_rs/0.76.0"

	maxRefreshConcurrency = 2

	// defaultMaxStale is the backstop refresh age: even a profile nobody
	// used gets re-checked this often, catching quota consumed outside CPA.
	defaultMaxStale = 6 * time.Hour
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
	// ProbeFresh sends one minimal "ping" request when a never-used long
	// window is detected, starting its countdown.
	ProbeFresh bool
	// MaxStale caps how old a snapshot may get before it is re-fetched even
	// when nothing else changed. Zero means defaultMaxStale.
	MaxStale time.Duration
}

// Refresher periodically pulls upstream quota snapshots into a Store.
type Refresher struct {
	client HostClient
	store  *Store
	config func() Config

	mu       sync.Mutex
	running  bool
	stop     chan struct{}
	wg       sync.WaitGroup
	probed   map[string]bool
	onDemand map[string]time.Time
}

// onDemandCooldown caps how often a stale snapshot is re-fetched on demand:
// concurrent picks share one attempt, and a failed attempt waits before the
// next try.
const onDemandCooldown = 5 * time.Minute

// NewRefresher returns a Refresher that is not yet running.
func NewRefresher(client HostClient, store *Store, config func() Config) *Refresher {
	return &Refresher{
		client: client,
		store:  store,
		config: config,
		probed: make(map[string]bool),
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

// RefreshOnce pulls a fresh quota snapshot for every eligible auth.
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
	sem := make(chan struct{}, maxRefreshConcurrency)
	var wg sync.WaitGroup
	for _, auth := range eligible {
		if !r.needsRefresh(auth, cfg, now) {
			continue
		}
		wg.Add(1)
		go func(a AuthEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r.refreshOne(a, cfg)
		}(auth)
	}
	wg.Wait()
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
}

// RefreshAuthNow re-fetches one auth's quota snapshot in the background.
// It is meant for snapshots whose window reset time has passed: the stored
// numbers are stale, and the next pick should see fresh ones instead of
// waiting for the next background cycle. Calls are single-flighted per auth
// with a cooldown, so a burst of picks triggers at most one fetch. Works
// even when background calibration is disabled (on-demand only).
func (r *Refresher) RefreshAuthNow(authID string) {
	if r == nil || authID == "" {
		return
	}
	cfg := r.config()
	if !cfg.Enabled {
		return
	}
	now := time.Now()
	r.mu.Lock()
	if r.onDemand == nil {
		r.onDemand = make(map[string]time.Time)
	}
	if last, ok := r.onDemand[authID]; ok && now.Sub(last) < onDemandCooldown {
		r.mu.Unlock()
		return
	}
	r.onDemand[authID] = now
	r.mu.Unlock()

	go func() {
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
		return
	}
	r.store.Set(snap)

	if cfg.ProbeFresh && snap.Fresh() {
		r.mu.Lock()
		already := r.probed[auth.ID]
		if !already {
			r.probed[auth.ID] = true
		}
		r.mu.Unlock()
		if !already {
			if creds, err := CredentialsForAuth(r.client, auth); err == nil {
				ProbeFreshWindow(r.client.DoHTTP, creds)
			}
		}
	} else if !snap.Fresh() {
		r.mu.Lock()
		delete(r.probed, auth.ID)
		r.mu.Unlock()
	}
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
