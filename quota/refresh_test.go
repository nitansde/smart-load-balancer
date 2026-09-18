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
}

func (s *stubHostClient) ListAuths() ([]AuthEntry, error) { return s.auths, nil }

func (s *stubHostClient) GetAuthJSON(authIndex string) (json.RawMessage, error) {
	return json.RawMessage(`{"access_token":"tok","account_id":"acc"}`), nil
}

func (s *stubHostClient) DoHTTP(req HTTPRequest) (HTTPResponse, error) {
	s.mu.Lock()
	s.doHTTPCalls++
	s.mu.Unlock()
	return HTTPResponse{StatusCode: 200, Body: s.usageBody}, nil
}

func (s *stubHostClient) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doHTTPCalls
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
	r := NewRefresher(client, store, func() Config { return Config{Enabled: true} })

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

func TestRefreshAuthNow_SingleFlight(t *testing.T) {
	now := time.Now()
	client := &stubHostClient{
		auths:     []AuthEntry{{ID: "a", AuthIndex: "0", Provider: "codex"}},
		usageBody: usageBodyWithReset(t, now.Add(5*time.Hour)),
	}
	store := NewStore()
	r := NewRefresher(client, store, func() Config { return Config{Enabled: true} })

	for i := 0; i < 10; i++ {
		r.RefreshAuthNow("a")
	}
	waitFor(t, 3*time.Second, func() bool {
		_, ok := store.Get("a")
		return ok
	}, "expected snapshot after RefreshAuthNow")
	// Cooldown may allow a second attempt only after 5 minutes; within the
	// test window there must be exactly one fetch.
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
	r := NewRefresher(client, store, func() Config { return Config{Enabled: false} })

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
	r := NewRefresher(client, store, func() Config { return Config{Enabled: true} })

	r.RefreshAuthNow("gone")
	waitFor(t, 3*time.Second, func() bool {
		_, ok := store.Get("gone")
		return !ok
	}, "expected vanished auth's snapshot to be removed")
}
