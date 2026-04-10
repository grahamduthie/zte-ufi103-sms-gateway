package main

// wifi_watchdog.go — monitors wlan0 connectivity and performs soft reconnects
// when the WiFi drops (as happens periodically with wpa_supplicant on this
// Qualcomm WCNSS PRONTO driver).
//
// Strategy: every 2 minutes check if wlan0 has an IP. If not:
//   1. Kill stale wpa_supplicant.
//   2. Bring wlan0 up.
//   3. Start a fresh wpa_supplicant (background).
//   4. Run udhcpc to get a new DHCP lease.
//
// Safety measures to prevent driver instability:
//   - Boot grace period: first 3 minutes after boot, no checks at all.
//   - Exponential backoff: each failed reconnect doubles the wait (up to 30 min).
//   - Hard limit: after 5 consecutive failed reconnects, stop trying entirely.
//   - wlan0 missing: if the device node is gone, stop trying (only reboot fixes it).
//
// We deliberately do NOT do rmmod/insmod here — multiple driver reload cycles
// put the pronto_wlan driver into an unrecoverable state (documented in
// DEVICE.md). A soft reconnect (wpa_supplicant restart only) is safe to repeat
// only when done sparingly.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"
)

const (
	wifiCheckInterval    = 120 * time.Second  // check every 2 minutes
	wifiBootGrace        = 3 * time.Minute    // don't check at all for 3 min after boot
	wifiMaxFailures      = 5                  // stop trying after this many consecutive failures
	wifiBackoffBase      = 60 * time.Second   // base backoff after a failed reconnect
	wifiBackoffMax       = 30 * time.Minute   // maximum backoff between attempts
	wpaStartupWait       = 15 * time.Second
	wpaSupplicantBin     = "/system/bin/wpa_supplicant"
	wpaSupplicantConf    = "/data/misc/wifi/wpa_supplicant.conf"
	wpaSupplicantSockets = "/data/misc/wifi/sockets"
	udhcpcBin            = "/system/bin/busybox"
	udhcpcScript         = "/data/sms-gateway/scripts/udhcpc.sh"
)

// runWiFiWatchdog runs until ctx is cancelled, periodically checking that
// wlan0 has an IP and attempting a soft reconnect if it doesn't.
func runWiFiWatchdog(ctx context.Context, logger *log.Logger) {
	t := time.NewTicker(wifiCheckInterval)
	defer t.Stop()

	// Boot grace period — don't touch WiFi for the first 3 minutes.
	grace := time.NewTimer(wifiBootGrace)
	defer grace.Stop()

	var (
		consecutiveFailures int
		nextAttemptAt       time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		// Skip checks during boot grace period.
		select {
		case <-grace.C:
		default:
			continue
		}

		// If we've hit the hard failure limit, stop trying entirely.
		if consecutiveFailures >= wifiMaxFailures {
			continue
		}

		// If we're still backing off from a recent failure, skip.
		if !nextAttemptAt.IsZero() && time.Now().Before(nextAttemptAt) {
			continue
		}

		// wlan0 device is completely gone — only a reboot fixes this.
		if !wlan0Exists() {
			logger.Printf("WiFi watchdog: wlan0 device missing — only a reboot can fix this")
			consecutiveFailures = wifiMaxFailures // stop trying
			continue
		}

		// wlan0 has an IP — connection is healthy, reset all failure counters.
		if wlan0HasIP() {
			consecutiveFailures = 0
			nextAttemptAt = time.Time{}
			continue
		}

		// wlan0 has no IP — attempt a soft reconnect.
		consecutiveFailures++
		logger.Printf("WiFi watchdog: wlan0 has no IP — attempting soft reconnect (attempt %d/%d)",
			consecutiveFailures, wifiMaxFailures)
		if err := softReconnectWiFi(logger); err != nil {
			logger.Printf("WiFi watchdog: soft reconnect failed: %v", err)
			// Exponential backoff: 60s, 120s, 240s, 480s, capped at 30 min.
			backoff := wifiBackoffBase * (1 << uint(consecutiveFailures-1))
			if backoff > wifiBackoffMax {
				backoff = wifiBackoffMax
			}
			nextAttemptAt = time.Now().Add(backoff)
			logger.Printf("WiFi watchdog: next attempt in %v", backoff.Truncate(time.Second))

			if consecutiveFailures >= wifiMaxFailures {
				logger.Printf("WiFi watchdog: %d consecutive failures — giving up until reboot", wifiMaxFailures)
			}
		} else {
			logger.Printf("WiFi watchdog: wlan0 reconnected successfully")
			consecutiveFailures = 0
			nextAttemptAt = time.Time{}
		}
	}
}

// wlan0Exists returns true if the wlan0 network device exists in the kernel.
func wlan0Exists() bool {
	_, err := os.Stat("/sys/class/net/wlan0")
	return err == nil
}

// wlan0HasIP returns true if wlan0 currently has an IPv4 address assigned.
func wlan0HasIP() bool {
	out, err := exec.Command("/system/bin/busybox", "ifconfig", "wlan0").Output()
	if err != nil {
		return false
	}
	return bytes.Contains(out, []byte("inet addr"))
}

// softReconnectWiFi kills any stale wpa_supplicant, starts a fresh one, and
// obtains a new DHCP lease. Does not touch the kernel driver module.
func softReconnectWiFi(logger *log.Logger) error {
	// Step 1: kill stale wpa_supplicant (it's in a disconnected state).
	exec.Command("/system/bin/busybox", "killall", "wpa_supplicant").Run()
	time.Sleep(2 * time.Second)

	// Step 2: bring wlan0 up (it may have gone down after the association loss).
	exec.Command("/system/bin/busybox", "ifconfig", "wlan0", "up").Run()
	time.Sleep(2 * time.Second)

	// Step 3: remove stale socket and start fresh wpa_supplicant.
	os.Remove(wpaSupplicantSockets + "/wlan0")
	cmd := exec.Command(wpaSupplicantBin,
		"-i", "wlan0",
		"-D", "nl80211",
		"-c", wpaSupplicantConf,
		"-O", wpaSupplicantSockets,
		"-B", // background
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wpa_supplicant start: %v (%s)", err, bytes.TrimSpace(out))
	}

	// Step 4: wait for association.
	logger.Printf("WiFi watchdog: wpa_supplicant started, waiting %s for association", wpaStartupWait)
	time.Sleep(wpaStartupWait)

	// Step 5: obtain DHCP lease.
	dhcp := exec.Command(udhcpcBin, "udhcpc",
		"-i", "wlan0",
		"-q", // quit after lease
		"-n", // exit if no lease
		"-s", udhcpcScript,
		"-x", "hostname:dongle",
	)
	if out, err := dhcp.CombinedOutput(); err != nil {
		return fmt.Errorf("udhcpc: %v (%s)", err, bytes.TrimSpace(out))
	}

	// Restore rndis0 IP in case it was affected.
	exec.Command("/system/bin/busybox", "ifconfig", "rndis0",
		"192.168.100.1", "netmask", "255.255.255.0", "up").Run()

	return nil
}
