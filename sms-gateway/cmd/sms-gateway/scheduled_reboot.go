package main

// scheduled_reboot.go — daily automatic reboot at a configured time.
//
// The pronto_wlan.ko WiFi driver on this device can crash spontaneously,
// making the web GUI unreachable until a reboot. A scheduled daily reboot
// ensures the worst-case outage is bounded (e.g. reboot at 03:00 → GUI
// recovers by ~03:02 at most ~24 hours after the driver crash).
//
// Configured via web.scheduled_reboot_time in config.json ("HH:MM" UK time).
// Empty string disables the feature.

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	"marlowfm.co.uk/sms-gateway/internal/config"
)

// runScheduledReboot ticks every minute and reboots the device once per day
// at the configured time. Uses approximate UK time (UTC+1) to match the rest
// of the codebase.
func runScheduledReboot(ctx context.Context, cfg *config.Config, logger *log.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()

	var lastRebootDate string // "YYYY-MM-DD" — prevents double-firing within the same minute

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		rebootTime := cfg.Web.ScheduledRebootTime
		if rebootTime == "" {
			continue
		}

		var wantHour, wantMin int
		if n, _ := fmt.Sscanf(rebootTime, "%d:%d", &wantHour, &wantMin); n != 2 {
			logger.Printf("Scheduled reboot: invalid time format %q (expected HH:MM) — skipping", rebootTime)
			continue
		}

		// Approximate UK time (UTC+1 / BST) — same convention as balance_checker.go.
		ukNow := time.Now().UTC().Add(time.Hour)
		today := ukNow.Format("2006-01-02")

		if ukNow.Hour() == wantHour && ukNow.Minute() == wantMin && today != lastRebootDate {
			lastRebootDate = today
			logger.Printf("Scheduled reboot: daily reboot triggered at %s UK time", rebootTime)
			exec.Command("/system/xbin/librank", "/system/bin/reboot").Run()
		}
	}
}
