package quota

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const sampleUsage = `{
	"plan_type": "pro",
	"rate_limit": {
		"primary_window": {
			"used_percent": 42.5,
			"limit_window_seconds": 18000,
			"reset_at": "2026-09-18T20:00:00Z"
		},
		"secondary_window": {
			"used_percent": 78.0,
			"limit_window_seconds": 604800,
			"reset_at": "2026-09-25T12:00:00Z"
		}
	}
}`

const freshUsage = `{
	"plan_type": "pro",
	"rate_limit": {
		"primary_window": {"used_percent": 0, "limit_window_seconds": 18000, "reset_at": "2026-09-18T20:00:00Z"},
		"secondary_window": {"used_percent": 0, "limit_window_seconds": 604800, "reset_at": "2026-09-25T12:00:00Z"}
	}
}`

const exhaustedUsage = `{
	"rate_limit": {
		"primary_window": {"used_percent": 100, "limit_reached": true, "limit_window_seconds": 18000},
		"secondary_window": {"used_percent": 95, "limit_window_seconds": 604800}
	}
}`

func TestParseUsage(t *testing.T) {
	parsed, err := ParseUsage([]byte(sampleUsage), time.Now())
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	if parsed.PlanType != "pro" {
		t.Errorf("plan type = %q", parsed.PlanType)
	}
	if parsed.FiveHour == nil || parsed.FiveHour.Kind != WindowFiveHour {
		t.Fatalf("five hour window missing: %+v", parsed.FiveHour)
	}
	if parsed.FiveHour.UsedPercent == nil || *parsed.FiveHour.UsedPercent != 42.5 {
		t.Errorf("five hour used = %v", parsed.FiveHour.UsedPercent)
	}
	if parsed.Long == nil || parsed.Long.Kind != WindowWeekly {
		t.Fatalf("long window missing: %+v", parsed.Long)
	}
	if parsed.Long.UsedPercent == nil || *parsed.Long.UsedPercent != 78.0 {
		t.Errorf("long used = %v", parsed.Long.UsedPercent)
	}
	wantReset := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if !parsed.Long.ResetAt.Equal(wantReset) {
		t.Errorf("reset_at = %v, want %v", parsed.Long.ResetAt, wantReset)
	}
}

func TestParseUsageExhausted(t *testing.T) {
	parsed, err := ParseUsage([]byte(exhaustedUsage), time.Now())
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	if parsed.FiveHour == nil || !parsed.FiveHour.Exhausted {
		t.Errorf("five hour should be exhausted: %+v", parsed.FiveHour)
	}
	if parsed.Long == nil || parsed.Long.Exhausted {
		t.Errorf("long window should not be exhausted: %+v", parsed.Long)
	}
}

func TestParseUsageCamelCase(t *testing.T) {
	raw := `{"rateLimit":{"primaryWindow":{"usedPercent":10,"limitWindowSeconds":18000},"secondaryWindow":{"usedPercent":20,"limitWindowSeconds":2592000}}}`
	parsed, err := ParseUsage([]byte(raw), time.Now())
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	if parsed.Long == nil || parsed.Long.Kind != WindowMonthly {
		t.Errorf("expected monthly long window, got %+v", parsed.Long)
	}
}

func TestSnapshotHelpers(t *testing.T) {
	used := 78.0
	snap := Snapshot{AuthID: "a1", Long: &Window{Kind: WindowWeekly, UsedPercent: &used}}
	if snap.Fresh() {
		t.Error("78% used should not be fresh")
	}
	if snap.LongUsedPercent() != 78.0 {
		t.Errorf("LongUsedPercent = %v", snap.LongUsedPercent())
	}
	zero := 0.0
	fresh := Snapshot{AuthID: "a2", Long: &Window{Kind: WindowWeekly, UsedPercent: &zero}}
	if !fresh.Fresh() {
		t.Error("0% used should be fresh")
	}
	unknown := Snapshot{AuthID: "a3"}
	if unknown.LongUsedPercent() != -1 {
		t.Errorf("unknown used should be -1, got %v", unknown.LongUsedPercent())
	}
	if unknown.Fresh() {
		t.Error("unknown should not be fresh")
	}
}

func TestExtractCodexCredentials(t *testing.T) {
	raw := json.RawMessage(`{"type":"codex","access_token":"tok123","refresh_token":"ref","account_id":"acc-1"}`)
	creds, err := ExtractCodexCredentials(raw)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if creds.AccessToken != "tok123" || creds.ChatGPTAccountID != "acc-1" {
		t.Errorf("creds = %+v", creds)
	}
	nested := json.RawMessage(`{"oauth":{"tokens":{"access_token":"deep-tok"}}}`)
	creds, err = ExtractCodexCredentials(nested)
	if err != nil || creds.AccessToken != "deep-tok" {
		t.Errorf("nested extract = %+v, err = %v", creds, err)
	}
	if _, err := ExtractCodexCredentials(json.RawMessage(`{}`)); err == nil {
		t.Error("expected error for missing access_token")
	}
}

// fakeHostClient is an in-memory HostClient for refresher tests.
type fakeHostClient struct {
	mu       sync.Mutex
	auths    []AuthEntry
	authJSON map[string]json.RawMessage
	usage    map[string][]byte
	requests []HTTPRequest
}

func (f *fakeHostClient) ListAuths() ([]AuthEntry, error) { return f.auths, nil }

func (f *fakeHostClient) GetAuthJSON(authIndex string) (json.RawMessage, error) {
	if raw, ok := f.authJSON[authIndex]; ok {
		return raw, nil
	}
	return nil, errors.New("no such auth")
}

func (f *fakeHostClient) DoHTTP(req HTTPRequest) (HTTPResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	// Match usage calls by URL; everything else is a probe.
	if req.URL == codexUsageEndpoint {
		token := strings.TrimPrefix(req.Headers["Authorization"], "Bearer ")
		if body, ok := f.usage[token]; ok && token != "" {
			return HTTPResponse{StatusCode: 200, Body: body}, nil
		}
		return HTTPResponse{StatusCode: 401}, nil
	}
	return HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
}

func testConfig() Config {
	return Config{Enabled: true, Providers: []string{"codex"}, Interval: time.Minute}
}

func TestRefresherPopulatesStore(t *testing.T) {
	client := &fakeHostClient{
		auths: []AuthEntry{
			{ID: "auth-1", AuthIndex: "0", Provider: "codex"},
			{ID: "auth-2", AuthIndex: "1", Provider: "codex"},
			{ID: "auth-3", AuthIndex: "2", Provider: "anthropic"}, // no endpoint: skipped
		},
		authJSON: map[string]json.RawMessage{
			"0": json.RawMessage(`{"access_token":"tok-a","account_id":"acc-a"}`),
			"1": json.RawMessage(`{"access_token":"tok-b","account_id":"acc-b"}`),
		},
		usage: map[string][]byte{
			"tok-a": []byte(sampleUsage),
			"tok-b": []byte(freshUsage),
		},
	}
	store := NewStore()
	r := NewRefresher(client, store, testConfig)
	r.RefreshOnce()

	snap, ok := store.Get("auth-1")
	if !ok {
		t.Fatal("auth-1 snapshot missing")
	}
	if snap.Long == nil || snap.LongUsedPercent() != 78.0 {
		t.Errorf("auth-1 long used = %v", snap.LongUsedPercent())
	}
	if snap.FiveHour == nil || snap.FiveHour.UsedPercent == nil || *snap.FiveHour.UsedPercent != 42.5 {
		t.Errorf("auth-1 five hour = %+v", snap.FiveHour)
	}
	snap2, ok := store.Get("auth-2")
	if !ok || !snap2.Fresh() {
		t.Errorf("auth-2 should be fresh: %+v ok=%v", snap2, ok)
	}
	if _, ok := store.Get("auth-3"); ok {
		t.Error("auth-3 (unsupported provider) should not have a snapshot")
	}
	// Auth header must carry the bearer token.
	found := false
	for _, req := range client.requests {
		if req.URL == codexUsageEndpoint && req.Headers["Authorization"] == "Bearer tok-a" {
			found = true
		}
	}
	if !found {
		t.Error("usage request missing bearer auth header")
	}
}

func TestRefresherProbeFresh(t *testing.T) {
	client := &fakeHostClient{
		auths:    []AuthEntry{{ID: "auth-1", AuthIndex: "0", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"0": json.RawMessage(`{"access_token":"tok-a"}`)},
		usage:    map[string][]byte{"tok-a": []byte(freshUsage)},
	}
	store := NewStore()
	cfg := testConfig()
	cfg.ProbeFresh = true
	r := NewRefresher(client, store, func() Config { return cfg })
	r.RefreshOnce()
	r.RefreshOnce() // second cycle must not probe again

	probes := 0
	for _, req := range client.requests {
		if req.URL == codexProbeEndpoint {
			probes++
			if req.Method != http.MethodPost {
				t.Errorf("probe method = %s", req.Method)
			}
		}
	}
	if probes != 1 {
		t.Errorf("probe sent %d times, want exactly 1", probes)
	}
}

func TestRefresherDisabled(t *testing.T) {
	client := &fakeHostClient{
		auths:    []AuthEntry{{ID: "auth-1", AuthIndex: "0", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"0": json.RawMessage(`{"access_token":"tok-a"}`)},
		usage:    map[string][]byte{"tok-a": []byte(sampleUsage)},
	}
	store := NewStore()
	cfg := testConfig()
	cfg.Enabled = false
	r := NewRefresher(client, store, func() Config { return cfg })
	r.RefreshOnce()
	if _, ok := store.Get("auth-1"); ok {
		t.Error("disabled refresher should not populate the store")
	}
	if len(client.requests) != 0 {
		t.Errorf("disabled refresher made %d requests", len(client.requests))
	}
}

func TestRefresherPrunesRemovedAuths(t *testing.T) {
	client := &fakeHostClient{
		auths:    []AuthEntry{{ID: "auth-1", AuthIndex: "0", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"0": json.RawMessage(`{"access_token":"tok-a"}`)},
		usage:    map[string][]byte{"tok-a": []byte(sampleUsage)},
	}
	store := NewStore()
	store.Set(Snapshot{AuthID: "auth-gone", Provider: "codex"})
	r := NewRefresher(client, store, testConfig)
	r.RefreshOnce()
	if _, ok := store.Get("auth-gone"); ok {
		t.Error("snapshot for removed auth should be pruned")
	}
}
