package main

import (
	"testing"
	"time"

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
