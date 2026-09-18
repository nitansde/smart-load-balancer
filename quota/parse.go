package quota

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	fiveHourSeconds = 18000
	weekSeconds     = 604800
	minMonthSeconds = 2419200
	maxMonthSeconds = 2678400
)

// Parsed is the quota-relevant subset of a wham/usage payload.
type Parsed struct {
	PlanType string
	FiveHour *Window
	Long     *Window // weekly or monthly
}

// ParseUsage parses a chatgpt.com/backend-api/wham/usage response body.
// Field names are matched case-insensitively in both snake_case and
// camelCase, mirroring the reference implementation.
func ParseUsage(raw []byte, now time.Time) (Parsed, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Parsed{}, err
	}
	parsed := Parsed{PlanType: getString(doc, "plan_type", "planType")}
	rateLimit, ok := getMap(doc, "rate_limit", "rateLimit")
	if !ok {
		return parsed, nil
	}
	for i, key := range []string{"primary_window", "secondary_window"} {
		alt := []string{"primaryWindow", "secondaryWindow"}[i]
		rawWindow, ok := getMap(rateLimit, key, alt)
		if !ok {
			continue
		}
		w := parseWindow(rawWindow, now, i)
		switch w.Kind {
		case WindowFiveHour:
			parsed.FiveHour = &w
		case WindowWeekly, WindowMonthly:
			parsed.Long = &w
		}
	}
	return parsed, nil
}

func parseWindow(raw map[string]any, now time.Time, order int) Window {
	w := Window{Kind: WindowFiveHour}
	if used, ok := getFloat64(raw, "used_percent", "usedPercent"); ok {
		w.UsedPercent = &used
		if used >= 100 {
			w.Exhausted = true
		}
	}
	if allowed, ok := getBool(raw, "allowed"); ok && !allowed {
		w.Exhausted = true
	}
	if reached, ok := getBool(raw, "limit_reached", "limitReached"); ok && reached {
		w.Exhausted = true
	}
	if resetAt, ok := getResetAt(raw, now); ok {
		w.ResetAt = resetAt
	}
	// Classify by limit window length; fall back to position when absent.
	if seconds, ok := getInt64(raw, "limit_window_seconds", "limitWindowSeconds"); ok {
		switch {
		case seconds == fiveHourSeconds:
			w.Kind = WindowFiveHour
		case seconds == weekSeconds:
			w.Kind = WindowWeekly
		case seconds >= minMonthSeconds && seconds <= maxMonthSeconds:
			w.Kind = WindowMonthly
		}
	} else if order == 1 {
		w.Kind = WindowWeekly
	}
	return w
}

func getResetAt(m map[string]any, now time.Time) (time.Time, bool) {
	if raw, ok := getAny(m, "reset_at", "resetAt"); ok {
		if s, ok := raw.(string); ok {
			if parsed, err := time.Parse(time.RFC3339, s); err == nil {
				return parsed, true
			}
		}
	}
	if seconds, ok := getFloat64(m, "reset_after_seconds", "resetAfterSeconds"); ok {
		return now.Add(time.Duration(seconds * float64(time.Second))), true
	}
	return time.Time{}, false
}

func getAny(m map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		for k, v := range m {
			if strings.EqualFold(k, key) {
				return v, true
			}
		}
	}
	return nil, false
}

func getMap(m map[string]any, keys ...string) (map[string]any, bool) {
	if v, ok := getAny(m, keys...); ok {
		if typed, ok := v.(map[string]any); ok {
			return typed, true
		}
	}
	return nil, false
}

func getString(m map[string]any, keys ...string) string {
	if v, ok := getAny(m, keys...); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getBool(m map[string]any, keys ...string) (bool, bool) {
	if v, ok := getAny(m, keys...); ok {
		if b, ok := v.(bool); ok {
			return b, true
		}
	}
	return false, false
}

func getFloat64(m map[string]any, keys ...string) (float64, bool) {
	if v, ok := getAny(m, keys...); ok {
		switch typed := v.(type) {
		case float64:
			return typed, true
		case int:
			return float64(typed), true
		case int64:
			return float64(typed), true
		case json.Number:
			if f, err := typed.Float64(); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func getInt64(m map[string]any, keys ...string) (int64, bool) {
	if v, ok := getAny(m, keys...); ok {
		switch typed := v.(type) {
		case float64:
			return int64(typed), true
		case int:
			return int64(typed), true
		case int64:
			return typed, true
		case json.Number:
			if i, err := typed.Int64(); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}
