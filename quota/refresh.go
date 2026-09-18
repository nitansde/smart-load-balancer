package quota

import (
	"encoding/json"
	"net/http"
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
}

// Refresher periodically pulls upstream quota snapshots into a Store.
type Refresher struct {
	client HostClient
	store  *Store
	config func() Config

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	wg      sync.WaitGroup
	probed  map[string]bool
}

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
	r.RefreshOnce()
	for {
		interval := r.config().Interval
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		select {
		case <-r.stop:
			return
		case <-time.After(interval):
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
	sem := make(chan struct{}, maxRefreshConcurrency)
	var wg sync.WaitGroup
	for _, auth := range eligible {
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

func (r *Refresher) refreshOne(auth AuthEntry, cfg Config) {
	endpoint, ok := usageEndpoints[strings.ToLower(auth.Provider)]
	if !ok {
		return
	}
	raw, err := r.client.GetAuthJSON(auth.AuthIndex)
	if err != nil || len(raw) == 0 {
		return
	}
	creds, err := ExtractCodexCredentials(raw)
	if err != nil || creds.AccessToken == "" {
		return
	}
	headers := map[string]string{
		"Authorization": "Bearer " + creds.AccessToken,
		"Content-Type":  "application/json",
		"User-Agent":    codexUserAgent,
	}
	if creds.ChatGPTAccountID != "" {
		headers["Chatgpt-Account-Id"] = creds.ChatGPTAccountID
	}
	resp, err := r.client.DoHTTP(HTTPRequest{Method: http.MethodGet, URL: endpoint, Headers: headers})
	if err != nil {
		return
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Token expired between host read and use; CPA refreshes tokens in
		// the background, so the next cycle picks up a fresh one.
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || len(resp.Body) == 0 {
		return
	}
	parsed, err := ParseUsage(resp.Body, time.Now())
	if err != nil {
		return
	}
	snap := Snapshot{
		AuthID:    auth.ID,
		Provider:  strings.ToLower(auth.Provider),
		FiveHour:  parsed.FiveHour,
		Long:      parsed.Long,
		PlanType:  parsed.PlanType,
		FetchedAt: time.Now(),
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
			r.probeFreshWindow(auth, creds)
		}
	} else if !snap.Fresh() {
		r.mu.Lock()
		delete(r.probed, auth.ID)
		r.mu.Unlock()
	}
}

// probeFreshWindow sends one minimal request to start a never-used long
// window's countdown. Best effort: failures just leave the window fresh.
func (r *Refresher) probeFreshWindow(auth AuthEntry, creds Credentials) {
	headers := map[string]string{
		"Authorization": "Bearer " + creds.AccessToken,
		"Content-Type":  "application/json",
		"User-Agent":    codexUserAgent,
	}
	if creds.ChatGPTAccountID != "" {
		headers["Chatgpt-Account-Id"] = creds.ChatGPTAccountID
	}
	_, _ = r.client.DoHTTP(HTTPRequest{
		Method:  http.MethodPost,
		URL:     codexProbeEndpoint,
		Headers: headers,
		Body:    []byte(codexProbePayload),
	})
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
