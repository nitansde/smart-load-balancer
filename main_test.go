package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/nitansde/smart-load-balancer/balancer"
	"github.com/nitansde/smart-load-balancer/quota"
)

// A monthly long-window reset that already passed must mark the account
// for calibration the same way a weekly one does.
func TestLongWindowResetPassed_Monthly(t *testing.T) {
	now := time.Now()
	up := 50.0
	passed := quota.Snapshot{
		AuthID: "a",
		Long:   &quota.Window{Kind: quota.WindowMonthly, UsedPercent: &up, ResetAt: now.Add(-time.Hour)},
	}
	if !longWindowResetPassed(passed, now) {
		t.Fatal("passed monthly reset must mark for calibration")
	}
	future := quota.Snapshot{
		AuthID: "a",
		Long:   &quota.Window{Kind: quota.WindowMonthly, UsedPercent: &up, ResetAt: now.Add(20 * 24 * time.Hour)},
	}
	if longWindowResetPassed(future, now) {
		t.Fatal("future monthly reset must not mark for calibration")
	}
}

// With the master switch off the plugin must decline scheduler picks so
// the host falls back to its default scheduler.
func TestDisabledPluginDeclinesPick(t *testing.T) {
	cfg := balancer.DefaultConfig()
	cfg.Enabled = false
	currentConfig.Store(cfg)
	defer currentConfig.Store(balancer.DefaultConfig())

	raw, err := pickAuth([]byte(`{}`))
	if err != nil {
		t.Fatalf("pickAuth returned error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("expected ok envelope, got error: %+v", env.Error)
	}
	var resp pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode pick response: %v", err)
	}
	if resp.Handled {
		t.Fatalf("disabled plugin handled the pick (auth %q); want decline", resp.AuthID)
	}
}

// The management UI exposes the master switch, the sticky TTL, and the
// 5h boost mode toggle — nothing else.
func TestConfigFieldsAreMinimal(t *testing.T) {
	fields := pluginRegistration().Metadata.ConfigFields
	if len(fields) != 3 {
		names := make([]string, 0, len(fields))
		for _, f := range fields {
			names = append(names, f.Name)
		}
		t.Fatalf("expected 3 config fields, got %d: %v", len(fields), names)
	}
	if fields[0].Name != "enabled" || fields[1].Name != "sticky_ttl_seconds" || fields[2].Name != "five_hour_boost" {
		t.Fatalf("unexpected config fields: %q, %q, %q", fields[0].Name, fields[1].Name, fields[2].Name)
	}
}

// FiveHourBoost end-to-end: an idle profile whose 5h window sits at 100%
// with a rolling reset must be marked during lookup, and the next pick
// must divert one real request to it to kick off its 5h countdown.
func TestFiveHourBoost_DivertsToIdleProfile(t *testing.T) {
	now := time.Now()
	cfg := balancer.DefaultConfig()
	cfg.FiveHourBoost = true
	currentConfig.Store(cfg)
	defer currentConfig.Store(balancer.DefaultConfig())

	fresh := balancer.NewDivertState()
	oldDivert := divertState
	divertState = fresh
	loadBalancer.SetDivertState(fresh)
	defer func() {
		divertState = oldDivert
		loadBalancer.SetDivertState(oldDivert)
	}()

	zero := 0.0
	half := 50.0
	ten := 10.0
	// The idle snapshot was fetched 3h ago with a rolling reset: idleness
	// must be judged at fetch time, or the kick would never fire.
	fetched := now.Add(-3 * time.Hour)
	quotaStore.Set(quota.Snapshot{
		AuthID:    "idle",
		Provider:  "codex",
		FetchedAt: fetched,
		FiveHour:  &quota.Window{Kind: quota.WindowFiveHour, UsedPercent: &zero, ResetAt: fetched.Add(5 * time.Hour)},
		Long:      &quota.Window{Kind: quota.WindowWeekly, UsedPercent: &ten, ResetAt: now.Add(6 * 24 * time.Hour)},
	})
	quotaStore.Set(quota.Snapshot{
		AuthID:   "busy",
		Provider: "codex",
		FiveHour: &quota.Window{Kind: quota.WindowFiveHour, UsedPercent: &half, ResetAt: now.Add(3 * time.Hour)},
		Long:     &quota.Window{Kind: quota.WindowWeekly, UsedPercent: &ten, ResetAt: now.Add(6 * 24 * time.Hour)},
	})
	defer quotaStore.Remove("idle")
	defer quotaStore.Remove("busy")

	r := &quotaResolver{now: time.Now}
	r.Lookup("idle", "codex")
	if fresh.Pending.Len() != 1 {
		t.Fatalf("idle profile should be marked for 5h kick, pending=%d", fresh.Pending.Len())
	}

	// Feed small-request stats so the borrowing key counts as small.
	for i := 0; i < 10; i++ {
		fresh.Stats.Observe("testkey", 10)
	}
	cands := []balancer.Candidate{{ID: "busy", Provider: "codex"}, {ID: "idle", Provider: "codex"}}
	got, handled := loadBalancer.PickWithQuota("testkey", cands, cfg, r)
	if !handled || got != "idle" {
		t.Fatalf("pick should divert to idle profile, got %q handled=%v", got, handled)
	}
}
