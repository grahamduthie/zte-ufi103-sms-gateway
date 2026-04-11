package main

// wifi_watchdog.go — monitors wlan0 connectivity and recovers from WiFi failures.
//
// Two distinct failure modes handled differently:
//
//   1. wlan0 exists but has no IP (wpa_supplicant lost association):
//      → Attempt a soft reconnect (kill stale wpa_supplicant, start fresh, DHCP).
//      → Exponential backoff, max 5 attempts, then stop soft reconnects.
//      → Soft reconnects are capped because repeated wpa_supplicant kill+restart
//        cycles on the Qualcomm WCNSS PRONTO driver accelerate its demise.
//
//   2. wlan0 device node disappears entirely (pronto_wlan.ko driver crash):
//      → The WCNSS firmware enters a corrupt state that cannot be recovered
//        without a hardware reset. rmmod/insmod does not help — the RF
//        subsystem ("iris") remains in a bad state regardless.
//      → After a 30-second confirmation wait (to avoid false positives during
//        boot or mode switches), trigger a full system reboot.
//      → Typical recovery time: ~2 min (detection + reboot + boot + WiFi setup).
//
// Note: we deliberately do NOT do rmmod/insmod here — multiple driver reload
// cycles put the pronto_wlan driver into an unrecoverable state even faster.
// Reboot is the only reliable fix once the driver has crashed.

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
	wifiCheckInterval    = 120 * time.Second // check every 2 minutes
	wifiBootGrace        = 3 * time.Minute   // don't check at all for 3 min after boot
	wifiMaxFailures      = 5                 // stop soft-reconnect attempts after this many consecutive failures
	wifiBackoffBase      = 60 * time.Second  // base backoff after a failed reconnect
	wifiBackoffMax       = 30 * time.Minute  // maximum backoff between attempts
	wifiDeadConfirmWait  = 30 * time.Second  // wait before confirming driver is truly dead
	wpaStartupWait       = 15 * time.Second
	wpaSupplicantBin     = "/system/bin/wpa_supplicant"
	wpaSupplicantConf    = "/data/misc/wifi/wpa_supplicant.conf"
	wpaSupplicantSockets = "/data/misc/wifi/sockets"
	udhcpcBin            = "/system/bin/busybox"
	udhcpcScript         = "/data/sms-gateway/scripts/udhcpc.sh"
)

// runWiFiWatchdog runs until ctx is cancelled, periodically checking that
// wlan0 has an IP and attempting a soft reconnect if it doesn't. If the
// driver crashes entirely (wlan0 device disappears), triggers a system reboot.
func runWiFiWatchdog(ctx context.Context, logger *log.Logger) {
	t := time.NewTicker(wifiCheckInterval)
	defer t.Stop()

	// Boot grace period — don't touch WiFi for the first 3 minutes.
	grace := time.NewTimer(wifiBootGrace)
	defer grace.Stop()

	var (
		consecutiveFailures int
		nextAttemptAt       time.Time
		graceExpired        bool
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		// Skip checks during boot grace period.
		// Use a boolean flag rather than re-reading grace.C: the timer channel
		// only sends one value, so repeated select { case <-grace.C: ... default: continue }
		// would always hit default after the first drain, silently disabling the
		// watchdog for the entire session.
		if !graceExpired {
			select {
			case <-grace.C:
				graceExpired = true
			default:
				continue
			}
		}

		// wlan0 device is completely gone — the pronto_wlan.ko driver has crashed.
		// Confirm after a short wait (avoids false positives during mode switches),
		// then reboot. This is the only reliable recovery for this hardware.
		if !wlan0Exists() {
			logger.Printf("WiFi watchdog: wlan0 device missing — waiting %s to confirm before rebooting", wifiDeadConfirmWait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wifiDeadConfirmWait):
			}
			if !wlan0Exists() {
				logger.Printf("WiFi watchdog: wlan0 still missing — pronto_wlan driver crashed, rebooting to recover")
				exec.Command("/system/xbin/librank", "/system/bin/reboot").Run()
				// Block until reboot takes effect; if it fails for any reason, stop watchdog.
				time.Sleep(60 * time.Second)
				return
			}
			logger.Printf("WiFi watchdog: wlan0 reappeared during confirmation window — not rebooting")
			continue
		}

		// wlan0 has an IP — connection is healthy, reset all failure counters.
		if wlan0HasIP() {
			consecutiveFailures = 0
			nextAttemptAt = time.Time{}
			continue
		}

		// wlan0 exists but has no IP.

		// If we've exhausted soft-reconnect attempts, keep monitoring but don't
		// attempt any more reconnects (further attempts risk destroying the driver).
		// The existence check above will catch it if the driver subsequently crashes.
		if consecutiveFailures >= wifiMaxFailures {
			continue
		}

		// If we're still backing off from a recent failure, skip.
		if !nextAttemptAt.IsZero() && time.Now().Before(nextAttemptAt) {
			continue
		}

		// Attempt a soft reconnect.
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
				logger.Printf("WiFi watchdog: %d consecutive soft-reconnect failures — suspending reconnect attempts (driver crash detection still active)", wifiMaxFailures)
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
		"-x", "hostname:sms-gateway",
	)
	if out, err := dhcp.CombinedOutput(); err != nil {
		return fmt.Errorf("udhcpc: %v (%s)", err, bytes.TrimSpace(out))
	}

	// Restore rndis0 IP in case it was affected.
	exec.Command("/system/bin/busybox", "ifconfig", "rndis0",
		"192.168.100.1", "netmask", "255.255.255.0", "up").Run()

	return nil
}
