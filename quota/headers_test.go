package quota

import (
	"net/http"
	"testing"
	"time"
)

func headerWindow(prefix string, used, minutes, resetAt string) http.Header {
	h := http.Header{}
	h.Set(prefix+"Used-Percent", used)
	h.Set(prefix+"Window-Minutes", minutes)
	if resetAt != "" {
		h.Set(prefix+"Reset-At", resetAt)
	}
	return h
}

func TestSnapshotFromHeaders_ParsesBothWindows(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	h := http.Header{}
	for k, v := range headerWindow("X-Codex-Primary-", "2", "10080", "1782951970") {
		h[k] = v
	}
	for k, v := range headerWindow("X-Codex-Secondary-", "45.5", "300", "") {
		h[k] = v
	}
	h.Set("X-Codex-Secondary-Reset-After-Seconds", "3600")
	h.Set("X-Codex-Plan-Type", "pro")

	snap, ok := SnapshotFromHeaders("a", "codex", h, now)
	if !ok {
		t.Fatal("expected quota data to be harvested")
	}
	if snap.Long == nil || snap.Long.Kind != WindowWeekly {
		t.Fatalf("weekly window not parsed: %+v", snap.Long)
	}
	if got := *snap.Long.UsedPercent; got != 2 {
		t.Fatalf("weekly used_percent = %v, want 2", got)
	}
	if !snap.Long.ResetAt.Equal(time.Unix(1782951970, 0)) {
		t.Fatalf("weekly reset_at = %v", snap.Long.ResetAt)
	}
	if snap.FiveHour == nil || snap.FiveHour.Kind != WindowFiveHour {
		t.Fatalf("five-hour window not parsed: %+v", snap.FiveHour)
	}
	if got := *snap.FiveHour.UsedPercent; got != 45.5 {
		t.Fatalf("five-hour used_percent = %v, want 45.5", got)
	}
	if want := now.Add(time.Hour); !snap.FiveHour.ResetAt.Equal(want) {
		t.Fatalf("five-hour reset = %v, want %v", snap.FiveHour.ResetAt, want)
	}
	if snap.PlanType != "pro" {
		t.Fatalf("plan type = %q, want pro", snap.PlanType)
	}
	if !snap.FetchedAt.Equal(now) {
		t.Fatalf("fetched_at = %v, want %v", snap.FetchedAt, now)
	}
}

func TestSnapshotFromHeaders_MonthlyWindow(t *testing.T) {
	h := headerWindow("X-Codex-Primary-", "10", "43200", "1782951970")
	snap, ok := SnapshotFromHeaders("a", "codex", h, time.Now())
	if !ok || snap.Long == nil || snap.Long.Kind != WindowMonthly {
		t.Fatalf("monthly window not classified: ok=%v snap=%+v", ok, snap)
	}
}

func TestSnapshotFromHeaders_NoQuotaHeaders(t *testing.T) {
	for _, h := range []http.Header{
		{},
		{"Content-Type": {"application/json"}},
		{"X-Codex-Primary-Used-Percent": {"50"}}, // no window minutes: unclassifiable
		{"X-Codex-Primary-Used-Percent": {"50"}, "X-Codex-Primary-Window-Minutes": {"999"}},
	} {
		if _, ok := SnapshotFromHeaders("a", "codex", h, time.Now()); ok {
			t.Fatalf("expected no harvest from headers %v", h)
		}
	}
}

func TestSnapshotFromHeaders_RejectsOutOfRange(t *testing.T) {
	h := headerWindow("X-Codex-Primary-", "150", "10080", "1782951970")
	if _, ok := SnapshotFromHeaders("a", "codex", h, time.Now()); ok {
		t.Fatal("used_percent > 100 must be rejected")
	}
}

func TestMergeSnapshot_OverlaysPerWindow(t *testing.T) {
	s := NewStore()
	up1, up2 := 10.0, 90.0
	s.Set(Snapshot{
		AuthID:   "a",
		Provider: "codex",
		FiveHour: &Window{Kind: WindowFiveHour, UsedPercent: &up1, ResetAt: time.Now().Add(time.Hour)},
		Long:     &Window{Kind: WindowWeekly, UsedPercent: &up1, ResetAt: time.Now().Add(24 * time.Hour)},
		PlanType: "pro",
	})
	// Header harvest only carries the weekly window this time.
	s.MergeSnapshot(Snapshot{
		AuthID:    "a",
		Provider:  "codex",
		Long:      &Window{Kind: WindowWeekly, UsedPercent: &up2, ResetAt: time.Now().Add(48 * time.Hour)},
		FetchedAt: time.Now(),
	})
	got, _ := s.Get("a")
	if *got.Long.UsedPercent != 90.0 {
		t.Fatalf("weekly not overlaid: %v", *got.Long.UsedPercent)
	}
	if got.FiveHour == nil || *got.FiveHour.UsedPercent != 10.0 {
		t.Fatalf("five-hour window must be preserved: %+v", got.FiveHour)
	}
	if got.PlanType != "pro" {
		t.Fatalf("plan type must be preserved: %q", got.PlanType)
	}
}

func TestMergeSnapshot_CreatesMissing(t *testing.T) {
	s := NewStore()
	up := 5.0
	s.MergeSnapshot(Snapshot{
		AuthID:    "b",
		Provider:  "codex",
		Long:      &Window{Kind: WindowWeekly, UsedPercent: &up},
		FetchedAt: time.Now(),
	})
	got, ok := s.Get("b")
	if !ok || got.Long == nil || *got.Long.UsedPercent != 5.0 {
		t.Fatalf("snapshot not created: %+v", got)
	}
}
