package quota

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

type stubHostClient struct {
	mu          sync.Mutex
	auths       []AuthEntry
	doHTTPCalls int
	usageBody   []byte
	// httpErr, when set, makes DoHTTP fail (fetch-failure paths).
	httpErr error
	// fetchTimes records when each DoHTTP call started.
	fetchTimes []time.Time
}

func (s *stubHostClient) ListAuths() ([]AuthEntry, error) { return s.auths, nil }

func (s *stubHostClient) GetAuthJSON(authIndex string) (json.RawMessage, error) {
	return json.RawMessage(`{"access_token":"tok","account_id":"acc"}`), nil
}

func (s *stubHostClient) DoHTTP(req HTTPRequest) (HTTPResponse, error) {
	s.mu.Lock()
	s.doHTTPCalls++
	s.fetchTimes = append(s.fetchTimes, time.Now())
	err := s.httpErr
	s.mu.Unlock()
	if err != nil {
		return HTTPResponse{}, err
	}
	return HTTPResponse{StatusCode: 200, Body: s.usageBody}, nil
}

func (s *stubHostClient) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doHTTPCalls
}

func (s *stubHostClient) times() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.fetchTimes...)
}

func usageBodyWithReset(t *testing.T, reset time.Time) []byte {
	t.Helper()
	body := fmt.Sprintf(`{
		"plan_type": "pro",
		"rate_limit": {
			"primary_window": {"used_percent": 10, "window_seconds": 18000,
				"reset_at": %q},
			"secondary_window": {"used_percent": 20, "window_seconds": 604800,
				"reset_at": %q}
		}
	}`, reset.Format(time.RFC3339), reset.Add(6*24*time.Hour).Format(time.RFC3339))
	return []byte(body)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestRefreshAuthNow_FetchesStaleSnapshot(t *testing.T) {
	now := time.Now()
	client := &stubHostClient{
		auths:     []AuthEntry{{ID: "a", AuthIndex: "0", Provider: "codex"}},
		usageBody: usageBodyWithReset(t, now.Add(5*time.Hour)),
	}
	store := NewStore()
	up := 80.0
	store.Set(Snapshot{
		AuthID:    "a",
		Provider:  "codex",
		Long:      &Window{Kind: WindowWeekly, UsedPercent: &up, ResetAt: now.Add(-time.Hour)},
		FetchedAt: now.Add(-time.Hour),
	})
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true, Interval: time.Hour} })

	r.RefreshAuthNow("a")

	waitFor(t, 3*time.Second, func() bool {
		snap, ok := store.Get("a")
		return ok && snap.FetchedAt.After(now)
	}, "expected fresh snapshot after RefreshAuthNow")
	if n := client.calls(); n != 1 {
		t.Fatalf("expected exactly 1 fetch, got %d", n)
	}
	snap, _ := store.Get("a")
	if snap.Long == nil || snap.Long.ResetAt.Before(now) {
		t.Fatalf("expected future long-window reset, got %+v", snap.Long)
	}
}

func TestRefreshAuthNow_DisabledWhenIntervalZero(t *testing.T) {
	client := &stubHostClient{
		auths:     []AuthEntry{{ID: "a", AuthIndex: "0", Provider: "codex"}},
		usageBody: usageBodyWithReset(t, time.Now().Add(5*time.Hour)),
	}
	store := NewStore()
	// Interval 0 means background calibration (and therefore on-demand
	// refresh) is disabled: the plugin must make no active requests.
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true} })
	r.RefreshAuthNow("a")
	time.Sleep(200 * time.Millisecond)
	if n := client.calls(); n != 0 {
		t.Fatalf("expected no fetch with interval 0, got %d", n)
	}
}

func TestRefreshAuthNow_SingleFlight(t *testing.T) {
	now := time.Now()
	client := &stubHostClient{
		auths:     []AuthEntry{{ID: "a", AuthIndex: "0", Provider: "codex"}},
		usageBody: usageBodyWithReset(t, now.Add(5*time.Hour)),
	}
	store := NewStore()
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true, Interval: time.Hour} })

	for i := 0; i < 10; i++ {
		r.RefreshAuthNow("a")
	}
	waitFor(t, 3*time.Second, func() bool {
		_, ok := store.Get("a")
		return ok
	}, "expected snapshot after RefreshAuthNow")
	// Cooldown may allow a second attempt only after the on-demand gap; within
	// the test window there must be exactly one fetch.
	time.Sleep(100 * time.Millisecond)
	if n := client.calls(); n != 1 {
		t.Fatalf("expected single-flighted fetch, got %d calls", n)
	}
}

func TestRefreshAuthNow_DisabledConfig(t *testing.T) {
	client := &stubHostClient{
		auths:     []AuthEntry{{ID: "a", AuthIndex: "0", Provider: "codex"}},
		usageBody: usageBodyWithReset(t, time.Now().Add(5*time.Hour)),
	}
	store := NewStore()
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: false} })

	r.RefreshAuthNow("a")
	time.Sleep(200 * time.Millisecond)
	if n := client.calls(); n != 0 {
		t.Fatalf("expected no fetch when disabled, got %d calls", n)
	}
	if _, ok := store.Get("a"); ok {
		t.Fatal("expected no snapshot stored when disabled")
	}
}

func TestRefreshAuthNow_UnknownAuthDropsSnapshot(t *testing.T) {
	client := &stubHostClient{auths: []AuthEntry{}}
	store := NewStore()
	up := 80.0
	store.Set(Snapshot{
		AuthID:   "gone",
		Provider: "codex",
		Long:     &Window{Kind: WindowWeekly, UsedPercent: &up},
	})
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true, Interval: time.Hour} })

	r.RefreshAuthNow("gone")
	waitFor(t, 3*time.Second, func() bool {
		_, ok := store.Get("gone")
		return !ok
	}, "expected vanished auth's snapshot to be removed")
}

func TestNeedsRefresh_FiveHourResetAloneDoesNotTrigger(t *testing.T) {
	now := time.Now()
	client := &stubHostClient{}
	store := NewStore()
	up := 20.0
	store.Set(Snapshot{
		AuthID:   "a",
		Provider: "codex",
		// Five-hour reset passed, but the weekly window is still current.
		FiveHour:  &Window{Kind: WindowFiveHour, UsedPercent: &up, ResetAt: now.Add(-time.Hour)},
		Long:      &Window{Kind: WindowWeekly, UsedPercent: &up, ResetAt: now.Add(6 * 24 * time.Hour)},
		FetchedAt: now.Add(-time.Hour),
	})
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true} })
	auth := AuthEntry{ID: "a", AuthIndex: "0", Provider: "codex"}

	if r.needsRefresh(auth, Config{Enabled: true}, now) {
		t.Fatal("a passed five-hour reset must not trigger a re-fetch on its own")
	}

	// A passed weekly reset still triggers.
	snap, _ := store.Get("a")
	snap.Long.ResetAt = now.Add(-time.Hour)
	store.Set(snap)
	if !r.needsRefresh(auth, Config{Enabled: true}, now) {
		t.Fatal("a passed weekly reset must trigger a re-fetch")
	}
}

// A failed quota fetch cools the auth down for five hours: an immediate
// retry from any path must be suppressed.
func TestRefresh_FailureCooldown(t *testing.T) {
	client := &stubHostClient{
		auths:   []AuthEntry{{ID: "a", AuthIndex: "0", Provider: "codex"}},
		httpErr: fmt.Errorf("boom"),
	}
	store := NewStore()
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true} })
	r.spread = time.Millisecond

	r.RefreshOnce()
	if n := client.calls(); n != 1 {
		t.Fatalf("expected 1 failed attempt, got %d", n)
	}
	if _, ok := store.Get("a"); ok {
		t.Fatal("failed fetch must not store a snapshot")
	}

	// Immediate retry from the periodic path must be skipped.
	r.RefreshOnce()
	if n := client.calls(); n != 1 {
		t.Fatalf("failed auth must cool down 5h, got %d calls", n)
	}

	// And from the on-demand path too.
	r.fetchGap = time.Millisecond
	r.RefreshAuthNow("a")
	time.Sleep(200 * time.Millisecond)
	if n := client.calls(); n != 1 {
		t.Fatalf("on-demand retry of failed auth must cool down 5h, got %d calls", n)
	}
}

// On-demand (reset-expiry) fetches for different profiles must be spaced at
// least fetchGap apart instead of bursting together.
func TestRefreshAuthNow_SpacedApart(t *testing.T) {
	now := time.Now()
	client := &stubHostClient{
		auths: []AuthEntry{
			{ID: "a", AuthIndex: "0", Provider: "codex"},
			{ID: "b", AuthIndex: "1", Provider: "codex"},
		},
		usageBody: usageBodyWithReset(t, now.Add(5*time.Hour)),
	}
	store := NewStore()
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true, Interval: time.Hour} })
	r.fetchGap = 200 * time.Millisecond

	r.RefreshAuthNow("a")
	r.RefreshAuthNow("b")

	waitFor(t, 5*time.Second, func() bool { return len(client.times()) == 2 },
		"expected both on-demand fetches to run")
	gap := client.times()[1].Sub(client.times()[0])
	if gap < 100*time.Millisecond {
		t.Fatalf("on-demand fetches must be spaced apart, gap = %v", gap)
	}
}

// One calibration cycle staggers its fetches across the spread window:
// with N profiles due, fetches start spread/N apart, the first immediately.
func TestRefreshOnce_SpreadsFetches(t *testing.T) {
	now := time.Now()
	client := &stubHostClient{
		auths: []AuthEntry{
			{ID: "a", AuthIndex: "0", Provider: "codex"},
			{ID: "b", AuthIndex: "1", Provider: "codex"},
			{ID: "c", AuthIndex: "2", Provider: "codex"},
		},
		usageBody: usageBodyWithReset(t, now.Add(5*time.Hour)),
	}
	store := NewStore()
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true} })
	r.spread = 300 * time.Millisecond

	start := time.Now()
	r.RefreshOnce()
	times := client.times()
	if len(times) != 3 {
		t.Fatalf("expected 3 staggered fetches, got %d", len(times))
	}
	if times[0].Sub(start) > 100*time.Millisecond {
		t.Fatalf("first fetch should start immediately, waited %v", times[0].Sub(start))
	}
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < 50*time.Millisecond {
			t.Fatalf("fetch %d started only %v after the previous one", i, d)
		}
	}
}

// The periodic cycle leaves alone a profile with a recently scheduled
// on-demand fetch instead of duplicating it.
func TestRefreshOnce_SkipsPendingOnDemand(t *testing.T) {
	now := time.Now()
	client := &stubHostClient{
		auths: []AuthEntry{
			{ID: "a", AuthIndex: "0", Provider: "codex"},
			{ID: "b", AuthIndex: "1", Provider: "codex"},
		},
		usageBody: usageBodyWithReset(t, now.Add(5*time.Hour)),
	}
	store := NewStore()
	r := NewRefresher(client, store, nil, func() Config { return Config{Enabled: true} })
	r.spread = time.Millisecond
	r.fetchGap = time.Hour
	r.mu.Lock()
	if r.onDemand == nil {
		r.onDemand = make(map[string]time.Time)
	}
	r.onDemand["a"] = time.Now() // scheduled moments ago, not yet run
	r.mu.Unlock()

	r.RefreshOnce()

	times := client.times()
	if len(times) != 1 {
		t.Fatalf("expected only b to be fetched, got %d fetches", len(times))
	}
	if _, ok := store.Get("b"); !ok {
		t.Fatal("expected b's snapshot to be fetched")
	}
	if _, ok := store.Get("a"); ok {
		t.Fatal("a has a pending on-demand fetch and must be left alone")
	}
}

func TestRefresherKickAfterResetWithHistory(t *testing.T) {
	client := &fakeHostClient{
		auths:    []AuthEntry{{ID: "auth-1", AuthIndex: "0", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"0": json.RawMessage(`{"access_token":"tok-a"}`)},
		// Window reset back to 100% with no countdown running.
		usage: map[string][]byte{"tok-a": []byte(resetUsage)},
	}
	store := NewStore()
	ledger := NewLedger()
	// The profile was heavily used in the previous window: local history
	// exists, but the new window's countdown hasn't started, so the kick
	// is still due. This is the whole point of the kick.
	ledger.Observe(UsageObservation{AuthID: "auth-1", Provider: "codex",
		TotalTokens: 500000, ObservedAt: time.Now().Add(-8 * 24 * time.Hour)})
	cfg := testConfig()
	cfg.ProbeFresh = true
	r := NewRefresher(client, store, ledger, func() Config { return cfg })
	r.spread = time.Millisecond
	r.RefreshOnce()

	probes := 0
	for _, req := range client.requests {
		if req.URL == codexProbeEndpoint {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("kick sent %d times, want exactly 1: a reset window needs its countdown started even with past usage", probes)
	}
	if e, ok := ledger.Get("auth-1"); !ok || !e.Probed {
		t.Error("ledger should be marked probed after the kick")
	}
}

func TestRefresherKickWhenLedgerClean(t *testing.T) {
	client := &fakeHostClient{
		auths:    []AuthEntry{{ID: "auth-1", AuthIndex: "0", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"0": json.RawMessage(`{"access_token":"tok-a"}`)},
		usage:    map[string][]byte{"tok-a": []byte(resetUsage)},
	}
	store := NewStore()
	ledger := NewLedger() // no history: never used through CPA
	cfg := testConfig()
	cfg.ProbeFresh = true
	r := NewRefresher(client, store, ledger, func() Config { return cfg })
	r.spread = time.Millisecond
	r.RefreshOnce()

	probes := 0
	for _, req := range client.requests {
		if req.URL == codexProbeEndpoint {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("kick sent %d times, want exactly 1 for a window with no countdown", probes)
	}
}

func TestRefresherNoKickWhenCountdownRunning(t *testing.T) {
	// Interval well short of the full window: the countdown is locked in
	// and shrinking. Built dynamically so the test doesn't rot.
	futureReset := time.Now().Add(6 * 24 * time.Hour).UTC().Format(time.RFC3339)
	usage := []byte(fmt.Sprintf(`{"plan_type":"pro","rate_limit":{` +
		`"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`,
		futureReset))
	client := &fakeHostClient{
		auths:    []AuthEntry{{ID: "auth-1", AuthIndex: "0", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"0": json.RawMessage(`{"access_token":"tok-a"}`)},
		usage:    map[string][]byte{"tok-a": usage},
	}
	store := NewStore()
	cfg := testConfig()
	cfg.ProbeFresh = true
	r := NewRefresher(client, store, NewLedger(), func() Config { return cfg })
	r.spread = time.Millisecond
	r.RefreshOnce()

	for _, req := range client.requests {
		if req.URL == codexProbeEndpoint {
			t.Fatal("kick sent while the window countdown is already running")
		}
	}
}

func TestRefresherKickWhenRollingReset(t *testing.T) {
	// reset_at ~one full window out: idle windows keep it rolling forward
	// on every fetch, so the countdown hasn't started — kick it.
	futureReset := time.Now().Add(7*24*time.Hour - time.Minute).UTC().Format(time.RFC3339)
	usage := []byte(fmt.Sprintf(`{"plan_type":"pro","rate_limit":{` +
		`"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":%q}}}`,
		futureReset))
	client := &fakeHostClient{
		auths:    []AuthEntry{{ID: "auth-1", AuthIndex: "0", Provider: "codex"}},
		authJSON: map[string]json.RawMessage{"0": json.RawMessage(`{"access_token":"tok-a"}`)},
		usage:    map[string][]byte{"tok-a": usage},
	}
	store := NewStore()
	cfg := testConfig()
	cfg.ProbeFresh = true
	r := NewRefresher(client, store, NewLedger(), func() Config { return cfg })
	r.spread = time.Millisecond
	r.RefreshOnce()

	probes := 0
	for _, req := range client.requests {
		if req.URL == codexProbeEndpoint {
			probes++
		}
	}
	if probes != 1 {
		t.Errorf("kick sent %d times, want exactly 1 for a rolling (not started) window", probes)
	}
}

func TestShouldKick_StaleSnapshot(t *testing.T) {
	zero := 0.0
	now := time.Now()
	full := 7 * 24 * time.Hour

	// Rolling reset_at (full interval) but usage observed after the fetch:
	// the snapshot is stale, that usage already started a countdown.
	r := NewRefresher(nil, NewStore(), NewLedger(), testConfig)
	r.ledger.Observe(UsageObservation{AuthID: "auth-1", ObservedAt: now.Add(time.Hour)})
	snap := Snapshot{AuthID: "auth-1", FetchedAt: now,
		Long: &Window{UsedPercent: &zero, ResetAt: now.Add(full)}}
	if r.shouldKick("auth-1", snap) {
		t.Error("shouldKick = true with usage newer than the snapshot")
	}

	// Rolling reset_at, usage before the fetch: snapshot is current, kick.
	r2 := NewRefresher(nil, NewStore(), NewLedger(), testConfig)
	r2.ledger.Observe(UsageObservation{AuthID: "auth-1", ObservedAt: now.Add(-time.Hour)})
	if !r2.shouldKick("auth-1", snap) {
		t.Error("shouldKick = false for a rolling window with no usage since the fetch")
	}

	// Shrinking interval: countdown already running, no kick.
	running := Snapshot{AuthID: "auth-1", FetchedAt: now,
		Long: &Window{UsedPercent: &zero, ResetAt: now.Add(full - 2*time.Hour)}}
	if r2.shouldKick("auth-1", running) {
		t.Error("shouldKick = true while the countdown interval is shrinking")
	}
}
