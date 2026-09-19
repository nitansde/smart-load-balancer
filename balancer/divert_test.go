package balancer

import (
	"net/http"
	"testing"
	"time"
)

func divertTestSetup(now time.Time) (*Balancer, *DivertState) {
	b := newWithClock(func() time.Time { return now })
	d := NewDivertState()
	b.SetDivertState(d)
	return b, d
}

// Feed one key with n samples of v tokens; every sample also lands in the
// global window.
func feedKey(s *KeyStats, key string, v int64, n int) {
	for i := 0; i < n; i++ {
		s.Observe(key, v)
	}
}

func TestKeyStats_WindowBoundedAt100(t *testing.T) {
	s := NewKeyStats()
	for i := int64(1); i <= 150; i++ {
		s.Observe("k", i)
	}
	mean, ok := s.MeanFor("k")
	if !ok {
		t.Fatal("expected mean for k")
	}
	// Only the most recent 100 samples (51..150) feed the mean.
	if mean != 100.5 {
		t.Fatalf("mean = %v, want 100.5 (last 100 of 150)", mean)
	}
	if _, ok := s.MeanFor("unknown"); ok {
		t.Fatal("unknown key must not have a mean")
	}
}

func TestKeyStats_IgnoresNonPositive(t *testing.T) {
	s := NewKeyStats()
	s.Observe("k", 0)
	s.Observe("k", -5)
	if _, ok := s.MeanFor("k"); ok {
		t.Fatal("non-positive samples must not create a key entry")
	}
	if _, ok := s.SmallThreshold(); ok {
		t.Fatal("non-positive samples must not feed the global window")
	}
}

func TestKeyStats_SmallThresholdP10(t *testing.T) {
	s := NewKeyStats()
	for i := int64(1); i <= 9; i++ {
		s.Observe("g", i*10)
	}
	if _, ok := s.SmallThreshold(); ok {
		t.Fatal("threshold needs at least 10 global samples")
	}
	s.Observe("g", 100) // 10 samples: 10..100
	threshold, ok := s.SmallThreshold()
	if !ok {
		t.Fatal("expected threshold with 10 samples")
	}
	// p10 of [10..100] (10 samples) = smallest = 10.
	if threshold != 10 {
		t.Fatalf("threshold = %d, want 10", threshold)
	}
	for i := int64(11); i <= 50; i++ {
		s.Observe("g", i*10)
	}
	threshold, _ = s.SmallThreshold()
	// p10 of 50 samples (10..500 step 10): index ceil(5)-1 = 4 -> 50.
	if threshold != 50 {
		t.Fatalf("threshold = %d, want 50", threshold)
	}
}

func TestKeyHashForValue_MatchesClientKeyHash(t *testing.T) {
	h := http.Header{"Authorization": []string{"Bearer abc123"}}
	if got := ClientKeyHash(h); got != KeyHashForValue("abc123") {
		t.Fatal("hash mismatch for bare value")
	}
	if got := ClientKeyHash(h); got != KeyHashForValue("Bearer abc123") {
		t.Fatal("hash mismatch for scheme-prefixed value")
	}
	if KeyHashForValue("") != "" || KeyHashForValue("   ") != "" {
		t.Fatal("empty value must hash to empty")
	}
}

func TestDivert_SmallKeyBorrowedNoSticky(t *testing.T) {
	now := time.Now()
	b, d := divertTestSetup(now)
	feedKey(d.Stats, "small", 10, 100) // mean 10, global p10 10 -> small
	feedKey(d.Stats, "big", 500, 40)
	d.Pending.Mark("target")

	cfg := quotaTestConfig()
	cfg.Sticky = true
	cands := candidates("a", "b", "target")
	got, handled := b.PickWithQuota("small", cands, cfg, stubResolver{})
	if !handled || got != "target" {
		t.Fatalf("small key should divert to target, got %q handled=%v", got, handled)
	}
	if _, ok := b.sticky["small"]; ok {
		t.Fatal("diverted pick must not create a sticky entry")
	}
	// Next pick goes through the normal path again.
	got, _ = b.PickWithQuota("small", cands, cfg, stubResolver{})
	if got != "a" {
		t.Fatalf("after divert, normal ordering should resume, got %q", got)
	}
	// The attempt was recorded, so the mark survives but is cooling down.
	if d.Pending.Len() != 1 {
		t.Fatalf("mark should survive a divert, len=%d", d.Pending.Len())
	}
}

func TestDivert_BigKeyWaitsForTimeout(t *testing.T) {
	now := time.Now()
	b, d := divertTestSetup(now)
	feedKey(d.Stats, "small", 10, 10)
	feedKey(d.Stats, "big", 500, 40) // mean 500 > p10 10
	d.Pending.Mark("target")

	cfg := quotaTestConfig()
	cands := candidates("a", "b", "target")
	got, _ := b.PickWithQuota("big", cands, cfg, stubResolver{})
	if got == "target" {
		t.Fatal("big key must not be diverted while the mark is fresh")
	}
	// After the timeout with no small-key traffic, any key may be borrowed.
	d.Pending.marks["target"].markedAt = now.Add(-7 * time.Hour)
	got, _ = b.PickWithQuota("big", cands, cfg, stubResolver{})
	if got != "target" {
		t.Fatalf("timed-out mark should divert any key, got %q", got)
	}
}

func TestDivert_AttemptCooldown(t *testing.T) {
	now := time.Now()
	b, d := divertTestSetup(now)
	feedKey(d.Stats, "small", 10, 100)
	d.Pending.Mark("target")

	cfg := quotaTestConfig()
	cands := candidates("a", "b", "target")
	if got, _ := b.PickWithQuota("small", cands, cfg, stubResolver{}); got != "target" {
		t.Fatalf("first pick should divert, got %q", got)
	}
	// Immediate retry must not borrow again for the same account.
	if got, _ := b.PickWithQuota("small", cands, cfg, stubResolver{}); got == "target" {
		t.Fatal("divert must cool down after an attempt")
	}
}

func TestDivert_OldestMarkWins(t *testing.T) {
	now := time.Now()
	b, d := divertTestSetup(now)
	feedKey(d.Stats, "small", 10, 100)
	d.Pending.Mark("t2")
	d.Pending.Mark("t1")
	d.Pending.marks["t2"].markedAt = now.Add(-time.Minute)
	d.Pending.marks["t1"].markedAt = now.Add(-time.Hour)

	cfg := quotaTestConfig()
	got, _ := b.PickWithQuota("small", candidates("a", "t1", "t2"), cfg, stubResolver{})
	if got != "t1" {
		t.Fatalf("oldest mark should win, got %q", got)
	}
}

func TestDivert_BlockedTargetSkipped(t *testing.T) {
	now := time.Now()
	b, d := divertTestSetup(now)
	feedKey(d.Stats, "small", 10, 100)
	d.Pending.Mark("target")

	cfg := quotaTestConfig()
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"target": {BlockedUntil: now.Add(time.Hour)},
	}}
	got, handled := b.PickWithQuota("small", candidates("a", "target"), cfg, resolver)
	if !handled || got != "a" {
		t.Fatalf("blocked target must be skipped, got %q handled=%v", got, handled)
	}
}

func TestDivert_NilStateDisables(t *testing.T) {
	b := New() // no divert state attached
	got, handled := b.PickWithQuota("k", candidates("a", "b"), quotaTestConfig(), stubResolver{})
	if !handled || got == "" {
		t.Fatalf("pick without divert state should work normally, got %q", got)
	}
}

func TestPendingCalibrations_MarkKeepsEarliest(t *testing.T) {
	p := NewPendingCalibrations()
	p.Mark("x")
	early := time.Now().Add(-2 * time.Hour)
	p.marks["x"].markedAt = early
	p.Mark("x") // re-mark must not move the timestamp forward
	if !p.marks["x"].markedAt.Equal(early) {
		t.Fatal("re-mark moved markedAt")
	}
	p.Clear("x")
	if p.Len() != 0 {
		t.Fatal("clear should drop the mark")
	}
}

func TestPendingCalibrations_Attempted(t *testing.T) {
	p := NewPendingCalibrations()
	p.Mark("x")
	p.Attempted("x")
	if p.marks["x"].lastAttempt.IsZero() {
		t.Fatal("attempted should stamp lastAttempt")
	}
	// Unknown IDs are ignored, not created.
	p.Attempted("ghost")
	if p.Len() != 1 {
		t.Fatal("attempted must not create marks")
	}
}
