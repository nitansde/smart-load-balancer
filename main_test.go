package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/nitansde/smart-load-balancer/balancer"
	"github.com/nitansde/smart-load-balancer/quota"
)

// A monthly long-window reset that already passed must trigger the same
// on-demand re-fetch as a weekly one.
func TestLongWindowResetPassed_Monthly(t *testing.T) {
	now := time.Now()
	up := 50.0
	passed := quota.Snapshot{
		AuthID: "a",
		Long:   &quota.Window{Kind: quota.WindowMonthly, UsedPercent: &up, ResetAt: now.Add(-time.Hour)},
	}
	if !longWindowResetPassed(passed, now) {
		t.Fatal("passed monthly reset must trigger a re-fetch")
	}
	future := quota.Snapshot{
		AuthID: "a",
		Long:   &quota.Window{Kind: quota.WindowMonthly, UsedPercent: &up, ResetAt: now.Add(20 * 24 * time.Hour)},
	}
	if longWindowResetPassed(future, now) {
		t.Fatal("future monthly reset must not trigger a re-fetch")
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

// The management UI only exposes the master switch and the sticky TTL.
func TestConfigFieldsAreMinimal(t *testing.T) {
	fields := pluginRegistration().Metadata.ConfigFields
	if len(fields) != 2 {
		names := make([]string, 0, len(fields))
		for _, f := range fields {
			names = append(names, f.Name)
		}
		t.Fatalf("expected 2 config fields, got %d: %v", len(fields), names)
	}
	if fields[0].Name != "enabled" || fields[1].Name != "sticky_ttl_seconds" {
		t.Fatalf("unexpected config fields: %q, %q", fields[0].Name, fields[1].Name)
	}
}
