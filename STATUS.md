# ZTE UFI103 SMS Gateway — Status & Quick Reference

*Last updated: 2026-04-10 12:30 BST*
*Device Serial: 19ce8266*

---

## What Works ✅

| Feature | Status | Notes |
|---------|--------|-------|
| Permanent root | ✅ | `/system/xbin/librank` = SUID rootshell (survives reboots) |
| SMS receive | ✅ | Polls SIM every 2s, imports to SQLite, deletes from SIM |
| SMS → Email | ✅ | **HTML emails** with Marlow FM logo, From/Received. Subject: `Text from +44... [DDMMYY-NNN]` |
| Email → SMS | ✅ | IMAP IDLE picks up replies; **Bug 13 fixed** — PDU mode + `promptCh` beats RILD injection |
| SMS send | ✅ | Sequential write approach (no intermediate reads) |
| Web UI | ✅ | **Port 80** with password gate — Dashboard, Received, Sent, Conversations, Compose, Settings |
| WiFi client | ⚠️ | Multi-network configured, but see "WiFi stability" below |
| Quoted-printable decode | ✅ | Smart quotes → regular apostrophes (multi-byte UTF-8 fixed) |
| Quote stripping | ✅ | Strips "On ... wrote:" blocks from email replies |
| SIM auto-unlock | ✅ | Proactive check at start of every SMS poll; SIM PIN lock removed |
| Shell injection fix | ✅ | `send_shell.go` deprecated + input validation on all SMS send paths |
| Automated tests | ✅ | **130 tests across 5 packages — all passing** |
| Boot persistence | ✅ | `qrngp` wrapper in `/init.target.rc` — triggers on `sys.boot_completed=1` |
| WiFi auto-setup on boot | ✅ | `start.sh` switches to client mode via `wifi-setup.sh` + dynamic DHCP |
| Single-instance guard | ✅ | PID file in `start.sh` prevents duplicate gateway processes |
| Log housekeeping | ✅ | Hourly rotation at 10 MB, WAL checkpoint, 90-day record pruning |
| Conversation pagination | ✅ | 30 per page with indexes on `(sender, received_at)` and `(to_number, created_at)` |
| Dashboard | ✅ | Monthly counts, last sent/received (UK time), gateway status, uptime |
| Settings controls | ✅ | Save config, Restart Gateway, Reboot Dongle, Shut Down Dongle |
| Auth gate | ✅ | Password set in `config.json` (`web.admin_password`) — not hardcoded |
| Logo embedding | ✅ | Logo loaded at startup, embedded as CID attachment in HTML emails |
| Date-based session IDs | ✅ | Format `DDMMYY-NNN` (e.g. `060426-001`), stored in `daemon_health` |
| Email threading | ✅ | Delivery confirmations use matching `Re: Text from +44... [DDMMYY-NNN]` subject |
| Restart page | ✅ | `/restarting` shows spinner, auto-redirects when gateway is back |
| GitHub security | ✅ | No passwords, phone numbers, or personal emails in repo or history |

## Active Investigation ⚠️

| Issue | Status | Notes |
|-------|--------|-------|
| USB mode cycling on host PC | ⚠️ Known | ModemManager probes `cdc-wdm0` (DIAG interface), triggers firmware USB re-enumeration. Does **not** affect gateway operation. Stops when ModemManager gives up. |
| **WiFi driver instability** | 🔴 **Active** | See below — this is the main ongoing issue |

## WiFi Driver Instability (Critical)

**Symptom**: The `pronto_wlan.ko` WiFi driver on this Qualcomm MSM8916 device
periodically crashes, causing `wlan0` to disappear entirely. When this happens,
the web GUI becomes unreachable and IMAP/SMTP disconnect.

**Root cause**: The Qualcomm WCNSS PRONTO driver has a known hardware/firmware
limitation — repeated wpa_supplicant restarts (kill + start) eventually put the
driver into an unrecoverable state. After 2-3 driver reload cycles in a single
boot session, `wlan0` vanishes and cannot be brought back without a reboot.

**What we've done to mitigate:**
1. **WiFi watchdog hardened** — checks every 2 minutes (not 45s), exponential
   backoff (60s → 30min), stops trying after 5 consecutive failures, detects
   missing `wlan0` device and backs off immediately.
2. **No driver reload in watchdog** — only soft reconnects (wpa_supplicant
   restart). Never does `rmmod`/`insmod` at runtime.
3. **Boot recovery** — a clean `adb reboot` restores the driver to a fresh state.

**Workaround when WiFi drops:**
```bash
# If the web UI is unreachable, reboot the dongle:
adb reboot
# Wait ~2 minutes for boot + WiFi setup + gateway start.
# The driver will be fresh and stable again.
```

**Long-term fix options** (not yet implemented):
- Replace `pronto_wlan.ko` with a newer/patched version if available
- Use external USB WiFi dongle instead of the onboard chip
- Accept periodic reboots as operational procedure (reliable once every 2-3 days)

## What Doesn't Work ❌

| Feature | Status | Notes |
|---------|--------|-------|
| WiFi management UI | ❌ | Not implemented — planned as part of `WIFI_AP_PLAN.md` |
| Host USB access | ⚠️ | Requires `sudo ip addr add 192.168.100.2/24 dev enx02030f556538` (RNDIS interface) |

## Current SIM

| Property | Value |
|----------|-------|
| Network | O2 - UK giffgaff |
| Number | +447700000002 |
| SMSC | +447356000010 |
| SIM PIN | ~~8837~~ **removed** (disabled via `AT+CLCK="SC",0,"8837"`) |
| Signal | ~-71 dBm, 3 bars |
| Storage | SM (SIM) = 20 slots |

---

## Quick Start

```bash
# Check device is connected
lsusb | grep 05c6          # → 05c6:90b4 Qualcomm Android

# Web UI is on port 80 with password gate
# Via WiFi: http://172.16.10.226/
# Via USB RNDIS: sudo ip addr add 192.168.100.2/24 dev enx02030f556538
#               http://192.168.100.1/

# Check status (needs auth cookie):
curl -s -c /tmp/gw.txt http://172.16.10.226/login -X POST -d "password=<your_password>"
curl -s -b /tmp/gw.txt http://172.16.10.226/status | python3 -m json.tool

# View live log:
adb shell "busybox tail -f /data/sms-gateway/sms-gateway.log"
```

## Build and Deploy a New Binary

```bash
cd /home/marlowfm/dongle/sms-gateway
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o sms-gateway ./cmd/sms-gateway

# Always push to .new first to avoid ETXTBSY (can't replace a running binary)
adb push sms-gateway /data/sms-gateway/sms-gateway.new

# The running gateway is owned by root (started by init service).
# Use librank to kill it, then move the new binary into place.
# start.sh's crash-restart loop will pick up the new binary within 10s.
adb shell "/system/xbin/librank /system/bin/busybox kill \$(busybox ps | busybox awk '/sms-gateway$/{print \$1}')"
sleep 2
adb shell "/system/xbin/librank /system/bin/busybox mv /data/sms-gateway/sms-gateway.new /data/sms-gateway/sms-gateway"
# Wait ~10s for start.sh to restart the gateway, then check:
sleep 12 && curl -s http://172.16.10.226/status | python3 -m json.tool
```

**IMPORTANT**: The gateway process is owned by root (uid=0, started by the init
service). The adb shell runs as uid=2000 and cannot kill root-owned processes
directly — use `librank` as shown above.

**IMPORTANT**: Never use raw `adb shell ... cat /dev/smd11` or `dd if=/dev/smd11`
while the daemon is running — stray readers steal modem responses and starve the daemon.

## Reboot and Recovery

```bash
# Clean reboot (new binary loads automatically on boot)
adb reboot

# If the gateway fails to start after boot, trigger the init service manually:
adb shell "setprop ctl.start sms-gw"
# If that doesn't work, the init service may be throttled (too many rapid exits).
# Reboot again to clear the throttle state.
```

## File Locations on Device

```
/data/sms-gateway/
├── sms-gateway          # Go binary (~16MB, ARM static)
├── sms-gateway.new      # Staging area for binary updates
├── config.json          # Credentials and settings (NOT in git — see .gitignore)
├── sms.db               # SQLite database (WAL mode)
├── sms-gateway.log      # Runtime log (rotated at 10MB by housekeeping goroutine)
├── sms-gateway.log.1    # Previous log file (one generation kept)
├── gateway.pid          # PID file for single-instance guard (root-owned)
└── scripts/             # WiFi setup scripts

/data/misc/wifi/
├── wpa_supplicant.conf  # WiFi client config (multi-network)
└── sockets/             # wpa_supplicant socket directory
```

## Key Files on Host PC

```
/home/marlowfm/dongle/
├── STATUS.md                  # ← You are here
├── DEVICE.md                  # Hardware specs, root, SIM, AT commands, RILD behaviour
├── GATEWAY.md                 # SMS gateway architecture, config, goroutines, data flow
├── BUGS.md                    # All bugs found and fixed (including WiFi driver issue)
├── SMS_MODEM_ARCHITECTURE.md  # CNMI/QMI SMS routing research and future fix options
├── REFACTOR_PLAN.md           # Completed refactoring plan (all items done)
├── WIFI_AP_PLAN.md            # Planned WiFi AP fallback feature
├── sms-gateway/               # Go source code (no credentials in git)
└── backup/                    # Partition backups (sbl1, aboot, boot, etc.)
```

---

*See also: `DEVICE.md` (hardware specs, root, SIM, AT commands), `GATEWAY.md` (architecture, config, data flow, testing), `BUGS.md` (all bugs and fixes, including WiFi driver instability), `SMS_MODEM_ARCHITECTURE.md` (CNMI/QMI SMS routing research and future fix options), `REFACTOR_PLAN.md` (completed refactoring items), `WIFI_AP_PLAN.md` (planned WiFi AP fallback with captive portal)*
