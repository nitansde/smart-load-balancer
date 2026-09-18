package quota

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SnapshotFromHeaders harvests quota numbers the host merged into the
// response headers of a completed request. Codex delivers its current
// window state inside the response stream (the codex.rate_limits event);
// the host parses that event into X-Codex-* headers and hands them to
// usage.handle along with the usage record. Every request therefore
// carries the freshest quota numbers for its own profile, with zero
// extra upstream fetches.
//
// Windows are classified strictly by their window_minutes value, never
// by primary/secondary position:
//
//	300   -> five-hour window
//	10080 -> weekly window (7 days)
//	43200 -> monthly window (30 days)
//
// It returns ok=false when the headers carry no usable quota data.
func SnapshotFromHeaders(authID, provider string, h http.Header, observedAt time.Time) (snap Snapshot, ok bool) {
	if authID == "" || len(h) == 0 {
		return Snapshot{}, false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	snap = Snapshot{AuthID: authID, Provider: provider, FetchedAt: observedAt}
	for _, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		w, good := parseHeaderWindow(h, prefix, observedAt)
		if !good {
			continue
		}
		switch w.Kind {
		case WindowFiveHour:
			snap.FiveHour = w
		case WindowWeekly, WindowMonthly:
			snap.Long = w
		}
		ok = true
	}
	if plan := strings.TrimSpace(h.Get("X-Codex-Plan-Type")); plan != "" {
		snap.PlanType = plan
		ok = true
	}
	if !ok {
		return Snapshot{}, false
	}
	return snap, true
}

// parseHeaderWindow reads one X-Codex-<Name>-* window from response headers.
func parseHeaderWindow(h http.Header, prefix string, observedAt time.Time) (*Window, bool) {
	usedRaw := strings.TrimSpace(h.Get(prefix + "Used-Percent"))
	minutesRaw := strings.TrimSpace(h.Get(prefix + "Window-Minutes"))
	if usedRaw == "" || minutesRaw == "" {
		return nil, false
	}
	used, err := strconv.ParseFloat(usedRaw, 64)
	if err != nil || used < 0 || used > 100 {
		return nil, false
	}
	minutes, err := strconv.Atoi(minutesRaw)
	if err != nil || minutes <= 0 {
		return nil, false
	}
	var kind WindowKind
	switch minutes {
	case 300:
		kind = WindowFiveHour
	case 10080:
		kind = WindowWeekly
	case 43200:
		kind = WindowMonthly
	default:
		return nil, false
	}
	w := &Window{Kind: kind, UsedPercent: &used}
	// Prefer the absolute reset time; fall back to the relative one.
	if resetRaw := strings.TrimSpace(h.Get(prefix + "Reset-At")); resetRaw != "" {
		if unix, err := strconv.ParseInt(resetRaw, 10, 64); err == nil && unix > 0 {
			w.ResetAt = time.Unix(unix, 0)
		}
	}
	if w.ResetAt.IsZero() {
		if afterRaw := strings.TrimSpace(h.Get(prefix + "Reset-After-Seconds")); afterRaw != "" {
			if secs, err := strconv.ParseInt(afterRaw, 10, 64); err == nil && secs >= 0 {
				w.ResetAt = observedAt.Add(time.Duration(secs) * time.Second)
			}
		}
	}
	return w, true
}
