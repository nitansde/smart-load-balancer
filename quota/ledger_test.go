package quota

import (
	"net/http"
	"testing"
	"time"
)

func TestLedgerFreshProfileHasNoEntry(t *testing.T) {
	l := NewLedger()
	if _, ok := l.Get("nope"); ok {
		t.Fatal("expected no ledger entry for a never-observed profile")
	}
}

func TestLedgerSuccessAccumulatesTokens(t *testing.T) {
	l := NewLedger()
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 100, ObservedAt: time.Now()})
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 250, ObservedAt: time.Now()})
	entry, ok := l.Get("a")
	if !ok || entry.ConsumedTokens != 350 || entry.Requests != 2 {
		t.Fatalf("expected 350 tokens/2 requests, got %+v (ok=%v)", entry, ok)
	}
	if entry.Fresh() {
		t.Fatal("a profile with observed usage is no longer fresh")
	}
}

func TestLedgerWeekly429BlocksUntilWindowReset(t *testing.T) {
	l := NewLedger()
	now := time.Now()
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 10, ObservedAt: now})
	l.Observe(UsageObservation{
		AuthID:      "a",
		Failed:      true,
		StatusCode:  429,
		FailureBody: `{"error":{"message":"You have reached your weekly usage limits"}}`,
		ObservedAt:  now,
	})
	entry, _ := l.Get("a")
	if entry.BlockReason != BlockReasonWeekly {
		t.Fatalf("expected weekly block reason, got %q", entry.BlockReason)
	}
	if !entry.Blocked(now.Add(time.Hour)) {
		t.Fatal("expected profile to stay blocked well past the failure")
	}
}

func TestLedgerAmbiguous429BacksOffFiveHours(t *testing.T) {
	l := NewLedger()
	now := time.Now()
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 10, ObservedAt: now})
	l.Observe(UsageObservation{
		AuthID:      "a",
		Failed:      true,
		StatusCode:  429,
		FailureBody: `{"error":{"message":"rate limit exceeded"}}`,
		ObservedAt:  now,
	})
	entry, _ := l.Get("a")
	if entry.BlockReason != BlockReasonFiveHour {
		t.Fatalf("expected five_hour block reason, got %q", entry.BlockReason)
	}
	if !entry.Blocked(now.Add(4 * time.Hour)) {
		t.Fatal("expected profile to stay blocked for ~5h")
	}
	if entry.Blocked(now.Add(6 * time.Hour)) {
		t.Fatal("expected profile to become eligible again after 5h backoff")
	}
}

func TestLedgerNonQuota429DoesNotBlock(t *testing.T) {
	l := NewLedger()
	now := time.Now()
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 10, ObservedAt: now})
	l.Observe(UsageObservation{
		AuthID:      "a",
		Failed:      true,
		StatusCode:  429,
		FailureBody: `{"error":{"message":"too many concurrent requests, try again"}}`,
		ObservedAt:  now,
	})
	entry, _ := l.Get("a")
	if entry.BlockReason != "" || entry.Blocked(now.Add(time.Minute)) {
		t.Fatalf("transient rate-limit must not block, got %+v", entry)
	}
}

func TestLedgerMarkProbedClearsFresh(t *testing.T) {
	l := NewLedger()
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 5, ObservedAt: time.Now()})
	l.MarkProbed("a")
	entry, _ := l.Get("a")
	if !entry.Probed || entry.Fresh() {
		t.Fatalf("expected probed and no longer fresh, got %+v", entry)
	}
}

func TestLedgerSuccessAfterBlockKeepsBlock(t *testing.T) {
	// A success record can arrive for a request that raced a 429; the block
	// must survive until its expiry.
	l := NewLedger()
	now := time.Now()
	l.Observe(UsageObservation{AuthID: "a", ObservedAt: now})
	l.Observe(UsageObservation{
		AuthID:      "a",
		Failed:      true,
		StatusCode:  429,
		FailureBody: "weekly usage limits reached",
		ObservedAt:  now,
	})
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 3, ObservedAt: now.Add(time.Minute)})
	entry, _ := l.Get("a")
	if entry.BlockReason != BlockReasonWeekly || !entry.Blocked(now.Add(time.Hour)) {
		t.Fatalf("block must survive later success records, got %+v", entry)
	}
}

func TestObservationQuotaDetection(t *testing.T) {
	obs := UsageObservation{AuthID: "a", Failed: true, StatusCode: 429}
	if _, ok := detectExhaustion(obs); !ok {
		t.Fatal("bare 429 should count as ambiguous quota signal")
	}
	obs.ResponseHeaders = http.Header{"X-Ratelimit-Reset": {"5"}}
	if _, ok := detectExhaustion(obs); !ok {
		t.Fatal("429 with rate-limit headers should still be an ambiguous quota signal")
	}
}

func TestLedgerWindowRollsAfterSevenDays(t *testing.T) {
	l := NewLedger()
	start := time.Now()
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 1000, ObservedAt: start})
	l.Observe(UsageObservation{AuthID: "a", TotalTokens: 500, ObservedAt: start.Add(8 * 24 * time.Hour)})
	entry, _ := l.Get("a")
	if entry.ConsumedTokens != 500 {
		t.Fatalf("expected weekly window to roll, got %d tokens", entry.ConsumedTokens)
	}
}
