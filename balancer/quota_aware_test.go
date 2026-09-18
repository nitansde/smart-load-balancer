package balancer

import (
	"testing"
	"time"
)

type stubResolver struct {
	infos map[string]QuotaInfo
}

func (s stubResolver) Lookup(authID, provider string) QuotaInfo {
	if qi, ok := s.infos[authID]; ok {
		return qi
	}
	return QuotaInfo{}
}

func pct(p float64) *float64 { return &p }

func candidates(ids ...string) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{ID: id, Provider: "codex"})
	}
	return out
}

func quotaTestConfig() Config {
	return Config{
		Sticky: false, Strategy: StrategyLeastConnections,
		WindowSeconds: 60, MaxInflightPerProfile: 3,
	}.WithDefaults()
}

func TestPickWithQuota_PriorityHigherFirst(t *testing.T) {
	b := New()
	cands := []Candidate{
		{ID: "low", Provider: "codex", Priority: 1},
		{ID: "high", Provider: "codex", Priority: 10},
		{ID: "mid", Provider: "codex", Priority: 5},
	}
	authID, ok := b.PickWithQuota("key", cands, quotaTestConfig(), stubResolver{})
	if !ok || authID != "high" {
		t.Fatalf("expected high priority profile, got %q (ok=%v)", authID, ok)
	}
}

func TestPickWithQuota_FillFirstBySnapshot(t *testing.T) {
	b := New()
	cands := candidates("a", "b", "c")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"a": {Known: true, UsedPercent: pct(20)},
		"b": {Known: true, UsedPercent: pct(80)},
		"c": {Known: true, UsedPercent: pct(50)},
	}}
	authID, ok := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if !ok || authID != "b" {
		t.Fatalf("expected most-used profile b, got %q (ok=%v)", authID, ok)
	}
}

func TestPickWithQuota_LedgerConsumedTokensFillFirst(t *testing.T) {
	b := New()
	cands := candidates("a", "b")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"a": {Known: true, ConsumedTokens: 100},
		"b": {Known: true, ConsumedTokens: 9000},
	}}
	authID, _ := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if authID != "b" {
		t.Fatalf("expected higher-consumed profile b, got %q", authID)
	}
}

func TestPickWithQuota_FreshLast(t *testing.T) {
	b := New()
	cands := candidates("used", "fresh")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"used":  {Known: true, ConsumedTokens: 500},
		"fresh": {Known: true, Fresh: true},
	}}
	authID, _ := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if authID != "used" {
		t.Fatalf("expected used profile before fresh one, got %q", authID)
	}
}

func TestPickWithQuota_BlockedExcluded(t *testing.T) {
	b := New()
	cands := candidates("blocked", "open")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"blocked": {Known: true, ConsumedTokens: 99999, BlockedUntil: time.Now().Add(time.Hour)},
		"open":    {Known: true, ConsumedTokens: 10},
	}}
	authID, ok := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if !ok || authID != "open" {
		t.Fatalf("expected open profile, got %q (ok=%v)", authID, ok)
	}
}

func TestPickWithQuota_AllBlockedDeclines(t *testing.T) {
	b := New()
	cands := candidates("a", "b")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"a": {Known: true, BlockedUntil: time.Now().Add(time.Hour)},
		"b": {Known: true, BlockedUntil: time.Now().Add(time.Hour)},
	}}
	if _, ok := b.PickWithQuota("key", cands, quotaTestConfig(), resolver); ok {
		t.Fatal("expected balancer to decline when all candidates blocked")
	}
}

func TestPickWithQuota_StickyOwnerAvoidance(t *testing.T) {
	b := New()
	cfg := quotaTestConfig()
	cfg.Sticky = true
	cands := candidates("x", "y", "z")
	// Other client keys claimed x and y, leaving z unclaimed.
	for _, key := range []string{"k1", "k2"} {
		for _, id := range []string{"x", "y"} {
			b.mu.Lock()
			b.sticky[key+id] = stickyEntry{authID: id, lastUsed: b.now()}
			b.mu.Unlock()
		}
	}
	authID, _ := b.PickWithQuota("newkey", cands, cfg, stubResolver{})
	if authID != "z" {
		t.Fatalf("expected unclaimed profile z, got %q", authID)
	}
}

func TestPickWithQuota_StickyFailoverOnExhaustion(t *testing.T) {
	b := New()
	cfg := quotaTestConfig()
	cfg.Sticky = true
	cands := candidates("a", "b")

	// First pick sticks to a (both unused: priority equal, loads equal,
	// tie-break decides; run enough to observe).
	first, _ := b.PickWithQuota("key", cands, cfg, stubResolver{})
	second, _ := b.PickWithQuota("key", cands, cfg, stubResolver{})
	if first != second {
		t.Fatalf("expected sticky to keep profile, got %q then %q", first, second)
	}

	// Now the sticky profile is exhausted: fail over to the other one.
	resolver := stubResolver{infos: map[string]QuotaInfo{
		first: {Known: true, BlockedUntil: time.Now().Add(time.Hour)},
	}}
	third, ok := b.PickWithQuota("key", cands, cfg, resolver)
	if !ok || third == first {
		t.Fatalf("expected failover from %q, got %q (ok=%v)", first, third, ok)
	}
}

func TestPickWithQuota_UserPrioritiesFirst(t *testing.T) {
	b := New()
	cfg := quotaTestConfig()
	cfg.QuotaPriorities = []string{"b", "a"}
	cands := candidates("a", "b", "c")
	// Even though all are unknown, user priorities put b before a before c.
	authID, _ := b.PickWithQuota("key", cands, cfg, stubResolver{})
	if authID != "b" {
		t.Fatalf("expected user-prioritized profile b, got %q", authID)
	}
}

func TestPickWithQuota_NilResolverKeepsOldBehavior(t *testing.T) {
	b := New()
	cands := candidates("a", "b")
	first, ok := b.Pick("key", cands, quotaTestConfig())
	if !ok {
		t.Fatal("expected pick to succeed without resolver")
	}
	second, _ := b.Pick("key2", cands, quotaTestConfig())
	if first == second {
		t.Fatal("expected deterministic tie-break to spread different keys")
	}
}

func TestPickWithQuota_ResetSoonestFirst(t *testing.T) {
	b := New()
	now := time.Now()
	cands := candidates("a", "b", "c")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"a": {Known: true, UsedPercent: pct(90), LongResetAt: now.Add(5 * 24 * time.Hour)},
		"b": {Known: true, UsedPercent: pct(10), LongResetAt: now.Add(24 * time.Hour)},
		"c": {Known: true, UsedPercent: pct(50), LongResetAt: now.Add(3 * 24 * time.Hour)},
	}}
	authID, ok := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if !ok || authID != "b" {
		t.Fatalf("expected soonest-reset profile b, got %q (ok=%v)", authID, ok)
	}
}

func TestPickWithQuota_ResetOutranksFillFirst(t *testing.T) {
	b := New()
	now := time.Now()
	cands := candidates("a", "b")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		// a resets much sooner but is less used: reset time wins.
		"a": {Known: true, UsedPercent: pct(10), LongResetAt: now.Add(24 * time.Hour)},
		"b": {Known: true, UsedPercent: pct(90), LongResetAt: now.Add(5 * 24 * time.Hour)},
	}}
	authID, _ := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if authID != "a" {
		t.Fatalf("expected sooner-reset profile a over more-used b, got %q", authID)
	}
}

func TestPickWithQuota_ResetWithinHourTiesToFillFirst(t *testing.T) {
	b := New()
	now := time.Now()
	cands := candidates("a", "b")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		// Resets 30 minutes apart count as the same moment: fill-first decides.
		"a": {Known: true, UsedPercent: pct(90), LongResetAt: now.Add(2 * time.Hour)},
		"b": {Known: true, UsedPercent: pct(10), LongResetAt: now.Add(2*time.Hour + 30*time.Minute)},
	}}
	authID, _ := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if authID != "a" {
		t.Fatalf("expected fill-first profile a within 1h reset tie, got %q", authID)
	}
}

func TestPickWithQuota_ResetBeyondHourNotTied(t *testing.T) {
	b := New()
	now := time.Now()
	cands := candidates("a", "b")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"a": {Known: true, UsedPercent: pct(10), LongResetAt: now.Add(2 * time.Hour)},
		"b": {Known: true, UsedPercent: pct(90), LongResetAt: now.Add(4 * time.Hour)},
	}}
	authID, _ := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if authID != "a" {
		t.Fatalf("expected sooner-reset profile a (>1h apart), got %q", authID)
	}
}

func TestPickWithQuota_KnownResetBeforeUnknown(t *testing.T) {
	b := New()
	now := time.Now()
	cands := candidates("a", "b")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		"a": {Known: true, UsedPercent: pct(90)},
		"b": {Known: true, UsedPercent: pct(10), LongResetAt: now.Add(5 * 24 * time.Hour)},
	}}
	authID, _ := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if authID != "b" {
		t.Fatalf("expected known-reset profile b before unknown-reset a, got %q", authID)
	}
}

func TestPickWithQuota_PastResetTreatedAsNow(t *testing.T) {
	b := New()
	now := time.Now()
	cands := candidates("a", "b")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		// a's window already renewed: the balancer's defensive clamp
		// treats a stale past reset as now (production resolvers
		// project it forward to the next window via NextReset).
		"a": {Known: true, UsedPercent: pct(10), LongResetAt: now.Add(-time.Minute)},
		"b": {Known: true, UsedPercent: pct(90), LongResetAt: now.Add(2 * time.Hour)},
	}}
	authID, _ := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if authID != "a" {
		t.Fatalf("expected already-reset profile a first, got %q", authID)
	}
}

func TestPickWithQuota_MonthlyResetSameAsWeekly(t *testing.T) {
	b := New()
	now := time.Now()
	cands := candidates("weekly", "monthly")
	resolver := stubResolver{infos: map[string]QuotaInfo{
		// "weekly" is on a weekly plan, "monthly" on a monthly plan; the
		// balancer must not care which kind the long window is, only
		// when it resets.
		"weekly":  {Known: true, UsedPercent: pct(80), LongResetAt: now.Add(20 * 24 * time.Hour)},
		"monthly": {Known: true, UsedPercent: pct(10), LongResetAt: now.Add(2 * 24 * time.Hour)},
	}}
	authID, _ := b.PickWithQuota("key", cands, quotaTestConfig(), resolver)
	if authID != "monthly" {
		t.Fatalf("expected sooner-reset monthly profile, got %q", authID)
	}
}
