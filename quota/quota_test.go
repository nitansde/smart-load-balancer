package quota

import (
	"encoding/json"
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

func TestWindowNextReset(t *testing.T) {
	now := time.Now()

	// Future reset passes through untouched.
	w := Window{Kind: WindowWeekly, ResetAt: now.Add(3 * 24 * time.Hour)}
	if got := w.NextReset(now); !got.Equal(w.ResetAt) {
		t.Fatalf("future reset changed: %v", got)
	}

	// Zero reset stays zero.
	if got := (Window{Kind: WindowWeekly}).NextReset(now); !got.IsZero() {
		t.Fatalf("zero reset changed: %v", got)
	}

	// Weekly reset 1 day ago rolls forward to 6 days from now.
	w = Window{Kind: WindowWeekly, ResetAt: now.Add(-24 * time.Hour)}
	if got := w.NextReset(now); got.Before(now) || got.After(now.Add(6*24*time.Hour+time.Minute)) {
		t.Fatalf("weekly roll-forward wrong: %v", got)
	} else {
		want := now.Add(6 * 24 * time.Hour)
		if got.Sub(want) > time.Minute || want.Sub(got) > time.Minute {
			t.Fatalf("weekly roll-forward = %v, want ~%v", got, want)
		}
	}

	// Very stale reset rolls forward by multiple periods.
	w = Window{Kind: WindowWeekly, ResetAt: now.Add(-20 * 24 * time.Hour)}
	got := w.NextReset(now)
	if got.Before(now) || got.After(now.Add(7*24*time.Hour)) {
		t.Fatalf("multi-period roll-forward wrong: %v", got)
	}

	// Monthly uses a 30-day period.
	w = Window{Kind: WindowMonthly, ResetAt: now.Add(-10 * 24 * time.Hour)}
	got = w.NextReset(now)
	want := now.Add(20 * 24 * time.Hour)
	if got.Sub(want) > time.Minute || want.Sub(got) > time.Minute {
		t.Fatalf("monthly roll-forward = %v, want ~%v", got, want)
	}
}

func TestParseUsageMonthly(t *testing.T) {
	now := time.Now()
	parsed, err := ParseUsage([]byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"primary_window": {"used_percent": 12.5, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
			"secondary_window": {"used_percent": 63.0, "limit_window_seconds": 2592000, "reset_after_seconds": 864000}
		}
	}`), now)
	if err != nil {
		t.Fatalf("ParseUsage: %v", err)
	}
	if parsed.Long == nil || parsed.Long.Kind != WindowMonthly {
		t.Fatalf("monthly window must land in Long: %+v", parsed.Long)
	}
	if got := *parsed.Long.UsedPercent; got != 63.0 {
		t.Fatalf("monthly used = %v, want 63", got)
	}
	if want := now.Add(864000 * time.Second); !parsed.Long.ResetAt.Equal(want) {
		t.Fatalf("monthly reset = %v, want %v", parsed.Long.ResetAt, want)
	}
}
