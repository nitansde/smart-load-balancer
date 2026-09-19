package balancer

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestBalancer() (*Balancer, *testClock) {
	clock := &testClock{now: time.Now()}
	return newWithClock(clock.Now), clock
}

func codexCandidates(n int) []Candidate {
	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Candidate{ID: fmt.Sprintf("codex-profile-%d", i), Provider: "codex"})
	}
	return out
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.WindowSeconds = 120
	cfg.MaxInflightPerProfile = 8
	return cfg
}

// The headline scenario: two API keys with 8 codex profiles available.
// Simultaneous requests must land on different profiles.
func TestTwoKeysSpreadAcrossProfiles(t *testing.T) {
	b, _ := newTestBalancer()
	cfg := testConfig()
	candidates := codexCandidates(8)

	keyA := ClientKeyHash(http.Header{"Authorization": []string{"Bearer key-aaa"}})
	keyB := ClientKeyHash(http.Header{"Authorization": []string{"Bearer key-bbb"}})
	if keyA == "" || keyB == "" || keyA == keyB {
		t.Fatalf("expected distinct key hashes, got %q and %q", keyA, keyB)
	}

	authA, okA := b.Pick(keyA, candidates, cfg)
	authB, okB := b.Pick(keyB, candidates, cfg)
	if !okA || !okB {
		t.Fatalf("picks not handled: %v %v", okA, okB)
	}
	if authA == authB {
		t.Fatalf("concurrent keys landed on the same profile %q; want spread", authA)
	}
}

// Many concurrent keys should spread evenly, not pile onto one profile.
func TestConcurrentKeysSpreadEvenly(t *testing.T) {
	b, _ := newTestBalancer()
	cfg := testConfig()
	cfg.Sticky = false // isolate the spreading behavior
	candidates := codexCandidates(8)

	const keys = 16
	var wg sync.WaitGroup
	results := make([]string, keys)
	for i := 0; i < keys; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keyHash := ClientKeyHash(http.Header{"Authorization": []string{fmt.Sprintf("Bearer key-%d", i)}})
			authID, ok := b.Pick(keyHash, candidates, cfg)
			if !ok {
				t.Errorf("pick %d not handled", i)
				return
			}
			results[i] = authID
		}(i)
	}
	wg.Wait()

	counts := map[string]int{}
	for _, id := range results {
		counts[id]++
	}
	for id, n := range counts {
		if n > 2 {
			t.Fatalf("profile %q received %d of %d concurrent picks; want even spread", id, n, keys)
		}
	}
}

// Sticky: sequential requests from the same key reuse the profile (cache affinity).
func TestStickyKeepsKeyOnSameProfile(t *testing.T) {
	b, _ := newTestBalancer()
	cfg := testConfig()
	candidates := codexCandidates(8)
	keyHash := ClientKeyHash(http.Header{"Authorization": []string{"Bearer stable-key"}})

	first, _ := b.Pick(keyHash, candidates, cfg)
	for i := 0; i < 5; i++ {
		authID, _ := b.Pick(keyHash, candidates, cfg)
		if authID != first {
			t.Fatalf("sequential pick %d went to %q, want sticky %q", i, authID, first)
		}
	}
}

// Sticky spillover: when the pinned profile is saturated, the key moves.
func TestStickySpillsOverWhenSaturated(t *testing.T) {
	b, clock := newTestBalancer()
	cfg := testConfig()
	cfg.MaxInflightPerProfile = 2
	candidates := codexCandidates(4)

	keyHash := ClientKeyHash(http.Header{"Authorization": []string{"Bearer main-key"}})
	pinned, _ := b.Pick(keyHash, candidates, cfg)

	// Saturate the pinned profile with other keys inside the window.
	for i := 0; i < cfg.MaxInflightPerProfile; i++ {
		other := ClientKeyHash(http.Header{"Authorization": []string{fmt.Sprintf("Bearer other-%d", i)}})
		b.Pick(other, []Candidate{{ID: pinned, Provider: "codex"}}, cfg)
	}
	clock.Advance(time.Second)

	moved, _ := b.Pick(keyHash, candidates, cfg)
	if moved == pinned {
		t.Fatalf("expected spillover away from saturated profile %q", pinned)
	}
}

// Sticky assignments expire after the TTL.
func TestStickyExpires(t *testing.T) {
	b, clock := newTestBalancer()
	cfg := testConfig()
	cfg.StickyTTLSeconds = 60
	candidates := codexCandidates(4)
	keyHash := ClientKeyHash(http.Header{"Authorization": []string{"Bearer ttl-key"}})

	if _, ok := b.Pick(keyHash, candidates, cfg); !ok {
		t.Fatal("pick not handled")
	}
	b.mu.Lock()
	if _, ok := b.sticky[keyHash]; !ok {
		b.mu.Unlock()
		t.Fatal("want sticky entry after pick")
	}
	b.mu.Unlock()

	clock.Advance(61 * time.Second)
	_ = b.LoadSnapshot(cfg) // triggers pruning

	b.mu.Lock()
	_, stillSticky := b.sticky[keyHash]
	b.mu.Unlock()
	if stillSticky {
		t.Fatal("sticky assignment should have expired after its TTL")
	}
}

// Sliding window: old picks stop influencing load.
func TestWindowPrunesOldPicks(t *testing.T) {
	b, clock := newTestBalancer()
	cfg := testConfig()
	cfg.Sticky = false
	cfg.WindowSeconds = 60
	candidates := codexCandidates(2)

	keyHash := ClientKeyHash(http.Header{"Authorization": []string{"Bearer w-key"}})
	first, _ := b.Pick(keyHash, candidates, cfg)
	if got := b.LoadSnapshot(cfg)[first]; got != 1 {
		t.Fatalf("want load 1 on %q, got %d", first, got)
	}
	clock.Advance(61 * time.Second)
	if got := len(b.LoadSnapshot(cfg)); got != 0 {
		t.Fatalf("want empty load snapshot after window, got %v", b.LoadSnapshot(cfg))
	}
}

// The strategy setting is ignored: a round-robin config must pick exactly
// like least-connections.
func TestStrategyIgnored(t *testing.T) {
	newCfg := func(strategy string) Config {
		cfg := testConfig()
		cfg.Strategy = strategy
		cfg.Sticky = false
		return cfg
	}
	candidates := codexCandidates(3)

	var seqLC, seqRR []string
	b1, _ := newTestBalancer()
	for i := 0; i < 6; i++ {
		got, _ := b1.Pick("", candidates, newCfg(StrategyLeastConnections))
		seqLC = append(seqLC, got)
	}
	b2, _ := newTestBalancer()
	for i := 0; i < 6; i++ {
		got, _ := b2.Pick("", candidates, newCfg(StrategyRoundRobin))
		seqRR = append(seqRR, got)
	}
	for i := range seqLC {
		if seqLC[i] != seqRR[i] {
			t.Fatalf("strategy must be ignored: least-connections[%d]=%q, round-robin[%d]=%q",
				i, seqLC[i], i, seqRR[i])
		}
	}
}

// Sticky TTL defaults to 4 hours.
func TestStickyTTLDefaultFourHours(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.StickyTTL(); got != 4*time.Hour {
		t.Fatalf("default sticky TTL = %v, want 4h", got)
	}
}

// Provider allowlist restricts balancing to matching providers.
func TestProviderFilter(t *testing.T) {
	b, _ := newTestBalancer()
	cfg := testConfig()
	cfg.Providers = []string{"codex"}
	candidates := []Candidate{
		{ID: "codex-1", Provider: "codex"},
		{ID: "gemini-1", Provider: "gemini"},
	}
	got, ok := b.Pick("k", candidates, cfg)
	if !ok || got != "codex-1" {
		t.Fatalf("got %q,%v; want codex-1,true", got, ok)
	}

	// No candidate matches the filter: decline so the host default applies.
	cfg.Providers = []string{"codex"}
	_, ok = b.Pick("k", []Candidate{{ID: "gemini-1", Provider: "gemini"}}, cfg)
	if ok {
		t.Fatal("want unhandled pick when no candidate matches the provider filter")
	}
}

func TestNoCandidatesUnhandled(t *testing.T) {
	b, _ := newTestBalancer()
	if _, ok := b.Pick("k", nil, testConfig()); ok {
		t.Fatal("want unhandled pick with no candidates")
	}
}

func TestSingleCandidate(t *testing.T) {
	b, _ := newTestBalancer()
	got, ok := b.Pick("k", []Candidate{{ID: "only", Provider: "codex"}}, testConfig())
	if !ok || got != "only" {
		t.Fatalf("got %q,%v; want only,true", got, ok)
	}
}

func TestClientKeyHash(t *testing.T) {
	bearer := ClientKeyHash(http.Header{"Authorization": []string{"Bearer secret-123"}})
	plain := ClientKeyHash(http.Header{"Authorization": []string{"secret-123"}})
	if bearer == "" || plain == "" {
		t.Fatal("want non-empty hashes")
	}
	if bearer != plain {
		t.Fatal("bearer scheme prefix should be stripped before hashing")
	}
	other := ClientKeyHash(http.Header{"Authorization": []string{"Bearer secret-456"}})
	if bearer == other {
		t.Fatal("different keys must hash differently")
	}
	if strings.Contains(bearer, "secret") {
		t.Fatal("hash must not contain raw key material")
	}
	if got := ClientKeyHash(nil); got != "" {
		t.Fatalf("want empty hash without headers, got %q", got)
	}
	apiKey := ClientKeyHash(http.Header{"X-Api-Key": []string{"k2"}})
	if apiKey == "" {
		t.Fatal("want hash from X-Api-Key fallback header")
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg := Config{}.WithDefaults()
	if cfg.Strategy != StrategyLeastConnections {
		t.Fatalf("want default strategy %q, got %q", StrategyLeastConnections, cfg.Strategy)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	bad := Config{Strategy: "magic"}.WithDefaults()
	if err := bad.Validate(); err == nil {
		t.Fatal("want validation error for unknown strategy")
	}
	def := DefaultConfig()
	if !def.Sticky || def.WindowSeconds != 120 || def.MaxInflightPerProfile != 8 {
		t.Fatalf("unexpected defaults: %+v", def)
	}
}

// Fill-first on ties: candidates tied on every ranking key keep ID
// order and the head always wins.
func TestFillFirstWithinTiedTier(t *testing.T) {
	b, _ := newTestBalancer()
	cfg := testConfig()
	cfg.Sticky = false // isolate the tie-break from the sticky fast path
	candidates := codexCandidates(3)

	for i := 0; i < 4; i++ {
		got, ok := b.Pick("key", candidates, cfg)
		if !ok {
			t.Fatalf("pick %d not handled", i)
		}
		// The second pick sees profile-0 with load 1, so the tier is
		// [profile-1, profile-2] and profile-1 wins; the third sees
		// [profile-2]; the fourth sees all loads tied and the
		// last-used preference keeps profile-2.
		want := []string{"codex-profile-0", "codex-profile-1", "codex-profile-2", "codex-profile-2"}[i]
		if got != want {
			t.Fatalf("pick %d = %q, want %q", i, got, want)
		}
	}
}

// With identical profiles, the second key takes the next ID after the
// first key's profile (no-conflict, then ID order), not a scattered one.
func TestSecondKeyTakesNextID(t *testing.T) {
	b, _ := newTestBalancer()
	cfg := testConfig()
	candidates := codexCandidates(4)

	a, _ := b.Pick("key-A", candidates, cfg)
	if a != "codex-profile-0" {
		t.Fatalf("A picked %q, want codex-profile-0", a)
	}
	bb, _ := b.Pick("key-B", candidates, cfg)
	if bb != "codex-profile-1" {
		t.Fatalf("B picked %q, want codex-profile-1", bb)
	}
}

// Natural ID order: numeric runs compare by value, so profile-2 comes
// before profile-10 (plain string comparison would put 10 first).
func TestNaturalIDOrder(t *testing.T) {
	b, _ := newTestBalancer()
	cfg := testConfig()
	cfg.Sticky = false
	candidates := []Candidate{
		{ID: "profile-10", Provider: "codex"},
		{ID: "profile-2", Provider: "codex"},
		{ID: "profile-1", Provider: "codex"},
	}
	want := []string{"profile-1", "profile-2", "profile-10"}
	for i, w := range want {
		got, ok := b.Pick("key", candidates, cfg)
		if !ok {
			t.Fatalf("pick %d not handled", i)
		}
		if got != w {
			t.Fatalf("pick %d = %q, want %q", i, got, w)
		}
	}
}

func TestNaturalIDLess(t *testing.T) {
	cases := []struct{ a, b string; want bool }{
		{"profile-2", "profile-10", true},
		{"profile-10", "profile-2", false},
		{"a", "b", true},
		{"a1", "a1", false},
		{"a01", "a1", false}, // equal value: fewer leading zeros first
		{"a1", "a01", true},
		{"9", "10", true},
	}
	for _, c := range cases {
		if got := naturalIDLess(c.a, c.b); got != c.want {
			t.Errorf("naturalIDLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// After the sticky TTL expires, the client prefers its most recently
// used profile (soft preference, not a pin).
func TestHistoryPreferenceAfterStickyExpiry(t *testing.T) {
	b, clock := newTestBalancer()
	cfg := testConfig()
	candidates := codexCandidates(4)

	if got, _ := b.Pick("key-A", candidates, cfg); got != "codex-profile-0" {
		t.Fatalf("A first pick = %q, want codex-profile-0", got)
	}
	if got, _ := b.Pick("key-B", candidates, cfg); got != "codex-profile-1" {
		t.Fatalf("B first pick = %q, want codex-profile-1", got)
	}
	clock.Advance(5 * time.Hour) // past the 4h sticky TTL

	// Without the preference both would fill-first to codex-profile-0.
	if got, _ := b.Pick("key-B", candidates, cfg); got != "codex-profile-1" {
		t.Fatalf("B after expiry = %q, want codex-profile-1 (last-used preference)", got)
	}
	if got, _ := b.Pick("key-A", candidates, cfg); got != "codex-profile-0" {
		t.Fatalf("A after expiry = %q, want codex-profile-0 (last-used preference)", got)
	}
}

// The last-used preference never overrides the no-conflict guarantee:
// a profile holding another key's active sticky stays avoided.
func TestHistoryDoesNotOverrideNoConflict(t *testing.T) {
	b, clock := newTestBalancer()
	cfg := testConfig()
	candidates := codexCandidates(8)

	b.Pick("key-A", candidates, cfg) // -> codex-profile-0
	b.Pick("key-B", candidates, cfg) // -> codex-profile-1
	clock.Advance(3 * time.Hour)
	b.Pick("key-A", candidates, cfg) // sticky refresh -> codex-profile-0
	clock.Advance(time.Hour + time.Minute)
	// B's sticky has expired; codex-profile-1 is free for C.
	if got, _ := b.Pick("key-C", candidates, cfg); got != "codex-profile-1" {
		t.Fatalf("C pick = %q, want codex-profile-1", got)
	}
	clock.Advance(time.Minute)
	// B's last-used is codex-profile-1, but C holds an active sticky there.
	if got, _ := b.Pick("key-B", candidates, cfg); got != "codex-profile-2" {
		t.Fatalf("B pick = %q, want codex-profile-2 (no-conflict beats last-used)", got)
	}
}
