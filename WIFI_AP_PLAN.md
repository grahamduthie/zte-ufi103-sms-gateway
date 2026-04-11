# WiFi AP Fallback — Design Plan
## Auto-Healing Connectivity for the ZTE UFI103 SMS Gateway

*Created: 2026-04-05*
*Revised: 2026-04-11 — single binary approach, save-and-reboot mode switching,
existing config schema, testing strategy added*
*Status: **COMPLETE** — fully implemented and deployed 2026-04-11*

> **Implementation notes** (deviations from plan):
> - Route structure: `/setup/add`, `/setup/delete`, `/setup/save` instead of single `POST /setup`
> - The `/generate_204` probe returns HTTP 302 redirect (not 200) — sending 302 correctly triggers
>   the OS captive portal UI, while 204 would signal "already authenticated"
> - The 120-second busybox timeout was not used; `start.sh` instead checks for IP after all
>   attempt paths (wlan0-already-connected, soft-reconnect, full wifi-setup.sh) and falls
>   back to AP only if wlan0 still has no IP after all paths complete
> - The `/api/wifi-preview`, `/api/wifi-diag`, and event log features from `WIFI_AP_TEST_PLAN.md`
>   were not implemented — they were reviewer suggestions, not in scope for this plan
> - **Added beyond plan**: WiFi management UI in the main Settings page — add, edit, reorder (↑/↓),
>   and remove networks without needing the setup captive portal

*Trigger: When the dongle boots and can't connect to its saved WiFi network,
it falls back to AP mode so a user can configure WiFi credentials via a web page.*

---

## Problem Statement

The dongle must be deployed standalone — powered by USB charger or battery,
not connected to any computer. In that mode:

1. **No SSH/ADB access** — the user can only reach the device via WiFi.
2. **WiFi credentials may change** — user moves to a new location, gets a new
   router, or changes their WiFi password.
3. **IP address is unknown** — DHCP assigns a dynamic IP; the user can't know
   what URL to browse to.
4. **No display or buttons** — the only user interface is the web GUI.

**Current behaviour**: `start.sh` runs `wifi-setup.sh` on boot, which switches
to WiFi client mode. If that fails (wrong password, no known network), the
device is unreachable — no fallback exists.

**Desired behaviour**: If client mode fails after a 120-second timeout, the
dongle falls back to AP mode and serves a WiFi setup page. The user connects
their phone/laptop to the `SMS-Gateway-Setup` hotspot, is redirected to the
setup page by a captive portal, configures the WiFi network, and reboots.
The dongle then boots normally into client mode.

---

## Architecture

### Single Binary Approach

The existing `sms-gateway` binary is extended with a `--setup-mode` flag.
There is **no separate wifi-manager binary**. `start.sh` remains the sole
boot entry point and decides which mode to launch.

```
start.sh (boot entry point, crash-restart loop)
  │
  ├─ [existing] PID file guard
  ├─ [existing] wifi-setup.sh → try client mode with 120s timeout
  │
  ├─ WiFi OK? ─── YES ──→ start sms-gateway (normal mode)
  │                        └─ all goroutines: SMS, IMAP, web UI, watchdog...
  │
  └─ WiFi failed? ─ NO ──→ wifi-ap-start.sh → switch to AP mode
                            └─ start sms-gateway --setup-mode
                                ├─ web server only (captive portal on :80)
                                ├─ NO /dev/smd11 session opened
                                ├─ NO SMS/IMAP/watchdog goroutines
                                └─ on save + reboot: librank reboot
```

**Why single binary?**
- The crash-restart loop and PID guard in `start.sh` already work; no need
  to duplicate process supervision in a second binary
- In setup mode, sms-gateway simply skips opening `/dev/smd11` and starts
  fewer goroutines — the code change is small
- One binary to build, push, and debug

**Why save-and-reboot instead of mid-session mode switching?**
- The `pronto_wlan.ko` driver can only safely handle ~3 `rmmod`/`insmod` cycles
  per boot before `wlan0` vanishes permanently (see BUGS.md, Bug 15–19)
- The WiFi watchdog now auto-reboots when `wlan0` disappears — a mid-session
  driver reload would race with the watchdog's 30-second confirmation timer
- Save-and-reboot is simpler and avoids both risks: write config, trigger
  `librank reboot`, let the next fresh boot handle client mode connection

---

## State Machine

```
BOOT (start.sh)
  │
  ├─ Run wifi-setup.sh (AP→CLIENT driver reload, wpa_supplicant, DHCP)
  │   └─ Wait up to 120 seconds for IPv4 on wlan0
  │
  ├─ Got IP? ─── YES ──→ Normal operation
  │                       sms-gateway (no flags)
  │                       All goroutines active including WiFi watchdog
  │
  └─ No IP? ─────────→ AP Fallback
                        run wifi-ap-start.sh
                        sms-gateway --setup-mode
                        │
                        ├─ Serve captive portal at 192.168.100.1:80
                        ├─ dnsmasq: all DNS → 192.168.100.1
                        ├─ iptables: all :80 → 192.168.100.1:80
                        ├─ User connects to "SMS-Gateway-Setup" hotspot
                        ├─ Browser redirected to setup page automatically
                        ├─ User adds/edits WiFi networks
                        ├─ Clicks "Save & Reboot"
                        └─ Gateway writes config → librank reboot
                            │
                            └─ BOOT again → wifi-setup.sh tries new network
                                ├─ Succeeds → Normal operation ✅
                                └─ Fails → AP Fallback again
```

---

## Detailed Design

### 1. Changes to `start.sh`

The WiFi section is extended with a 120-second timeout and an AP fallback:

```bash
# ── WiFi setup ───────────────────────────────────────────────────────────────

# Attempt WiFi client mode. wifi-setup.sh does the driver reload (AP→CLIENT),
# starts wpa_supplicant, and runs udhcpc. It exits 0 on success, non-zero if
# DHCP fails within its internal timeout.
#
# We give it 120 seconds total (driver load ~10s, wpa_supplicant association
# ~30s, DHCP ~10s — observed boot takes 90-120s in practice).

if busybox timeout 120 sh $GW_DIR/scripts/wifi-setup.sh >> "$LOG" 2>&1; then
    echo "[$(date)] start.sh: WiFi client mode OK" >> "$LOG"
    WIFI_MODE=client
else
    echo "[$(date)] start.sh: WiFi client failed — falling back to AP mode" >> "$LOG"
    sh $GW_DIR/scripts/wifi-ap-start.sh >> "$LOG" 2>&1
    WIFI_MODE=ap
fi

# ── Launch gateway ────────────────────────────────────────────────────────────

if [ "$WIFI_MODE" = "ap" ]; then
    exec /system/xbin/librank /system/bin/sh -c \
        "$GW_BIN --config $CONFIG --setup-mode >> $LOG 2>&1"
else
    exec /system/xbin/librank /system/bin/sh -c \
        "$GW_BIN --config $CONFIG >> $LOG 2>&1"
fi
```

### 2. New `wifi-ap-start.sh`

Reverses `wifi-setup.sh`: tears down client mode and brings up AP mode with
hostapd + dnsmasq + iptables captive portal rules.

```bash
#!/system/bin/sh
# wifi-ap-start.sh — switch wlan0 to AP (hostapd) mode for WiFi setup portal
set -e

GW_DIR=/data/sms-gateway

# ── Tear down client mode ─────────────────────────────────────────────────────
busybox killall wpa_supplicant 2>/dev/null || true
busybox sleep 2

# ── Driver reload (CLIENT → AP) ───────────────────────────────────────────────
busybox ifconfig wlan0 down 2>/dev/null || true
rmmod wlan 2>/dev/null || true
busybox sleep 3
insmod /system/lib/modules/pronto/pronto_wlan.ko
busybox sleep 5

# ── Bridge setup ─────────────────────────────────────────────────────────────
brctl addbr bridge1 2>/dev/null || true
brctl addif bridge1 wlan0 2>/dev/null || true
brctl addif bridge1 rndis0 2>/dev/null || true
busybox ifconfig bridge1 192.168.100.1 netmask 255.255.255.0 up
busybox ifconfig rndis0 192.168.100.1 netmask 255.255.255.0 up

# ── hostapd ───────────────────────────────────────────────────────────────────
# hostapd.conf is generated at runtime with SSID from config (see gateway)
/system/bin/hostapd -e /data/misc/wifi/entropy.bin \
    $GW_DIR/scripts/hostapd-setup.conf -B

busybox sleep 2

# ── dnsmasq: DHCP + wildcard DNS redirect (captive portal) ───────────────────
busybox killall dnsmasq 2>/dev/null || true
busybox sleep 1
dnsmasq \
    --keep-in-foreground \
    --interface=bridge1 \
    --dhcp-range=192.168.100.2,192.168.100.254,1h \
    --address=/#/192.168.100.1 \
    --no-resolv \
    --no-hosts \
    --pid-file=/data/sms-gateway/dnsmasq.pid \
    --log-facility=/dev/null &

# ── iptables: captive portal redirect ────────────────────────────────────────
iptables -t nat -F PREROUTING 2>/dev/null || true
iptables -t nat -A PREROUTING -i wlan0 -p tcp --dport 80 \
    -j DNAT --to-destination 192.168.100.1:80
iptables -t nat -A PREROUTING -i wlan0 -p tcp --dport 443 \
    -j DNAT --to-destination 192.168.100.1:80

echo "[$(date)] wifi-ap-start.sh: AP mode ready at 192.168.100.1"
```

### 3. `hostapd-setup.conf`

Generated at runtime (or pre-written) with the setup SSID:

```
interface=wlan0
driver=nl80211
ssid=SMS-Gateway-Setup
channel=6
hw_mode=g
wpa=2
wpa_passphrase=smsgateway
wpa_key_mgmt=WPA-PSK
rsn_pairwise=CCMP
```

The SSID is always `SMS-Gateway-Setup`. The password `smsgateway` is fixed
(users just need to find and connect to this network — it's a setup network,
not a security boundary). Both are hardcoded; there's no reason to make them
configurable.

### 4. `sms-gateway --setup-mode`

When started with `--setup-mode`, the gateway:

- **Does not open `/dev/smd11`** — no AT session, no modem interaction
- **Does not start**: SMS poller, send queue, IMAP IDLE, WiFi watchdog, SIM
  keepalive, balance checker, signal poller, or housekeeping
- **Does start**: a minimal web server on `:80` serving only the setup UI

The setup web server handles:

| Route | Purpose |
|-------|---------|
| `GET /` | Redirect to `/setup` (captive portal entry) |
| `GET /setup` | WiFi setup page |
| `POST /setup` | Save new network config, trigger reboot |
| `GET /generate_204` | Android captive portal detection — returns 204 |
| `GET /hotspot-detect.html` | iOS captive portal detection — redirect to `/setup` |
| `GET /ncsi.txt` | Windows captive portal detection — redirect to `/setup` |
| `GET /connectivity-check.gstatic.com` | Additional Android probe |

### 5. WiFi Setup Page

The setup page uses the existing PicoCSS styling and Marlow FM branding:

```
┌──────────────────────────────────────────┐
│  [Marlow FM logo]  SMS Gateway WiFi Setup │
├──────────────────────────────────────────┤
│                                          │
│  The gateway could not connect to any    │
│  saved WiFi network. Add one below.      │
│                                          │
│  ── Saved Networks ──────────────────    │
│  • MyHomeWifi [priority 1]  [Remove]     │
│  • OldNetwork  [priority 2]  [Remove]    │
│                                          │
│  ── Add Network ─────────────────────    │
│  WiFi Name (SSID):                       │
│  [________________________]              │
│                                          │
│  Password:                               │
│  [________________________] [👁]         │
│                                          │
│  Security: [WPA2 ▾]                      │
│                                          │
│  [Add Network]                           │
│                                          │
│  ─────────────────────────────────────   │
│  [Save & Reboot]                         │
│                                          │
│  The dongle will reboot and attempt to   │
│  connect. This takes ~2 minutes.         │
└──────────────────────────────────────────┘
```

**Behaviour on "Save & Reboot":**
1. Validate: at least one network with non-empty SSID and password ≥ 8 chars
2. Write updated `wifi.networks` array to `config.json`
3. Re-generate `/data/misc/wifi/wpa_supplicant.conf` from the new network list
4. Return a "Rebooting..." page with Marlow FM branding (same as existing
   `/restarting` page)
5. Trigger `exec.Command("/system/xbin/librank", "/system/bin/reboot").Run()`

### 6. Config Schema

Uses the **existing** `WiFiConfig` struct — no changes to `config.go`:

```go
type WiFiConfig struct {
    Mode     string       `json:"mode"`      // "client" or "ap" (informational)
    Networks []WiFiNetCfg `json:"networks"`  // existing multi-network array
}

type WiFiNetCfg struct {
    SSID     string `json:"ssid"`
    Password string `json:"password"`
    Security string `json:"security"`  // "WPA2", "WPA", "open"
    Priority int    `json:"priority"`
}
```

The setup page populates and saves this array directly. `wifi-setup.sh`
generates `wpa_supplicant.conf` from `config.json` at boot (already partly
does this — will need extending to read from `wifi.networks`).

### 7. `force_ap_mode` Config Flag

A `wifi.force_ap_mode` boolean flag is added to `WiFiConfig` for testing:

```go
type WiFiConfig struct {
    Mode        string       `json:"mode"`
    Networks    []WiFiNetCfg `json:"networks"`
    ForceAPMode bool         `json:"force_ap_mode"` // testing only
}
```

When `force_ap_mode: true`, `start.sh` skips the WiFi client attempt entirely
and goes straight to AP mode. This is the primary mechanism for testing the
AP fallback at a location with a known WiFi network (see Testing section).
The flag is cleared automatically after the gateway writes new WiFi config
during setup (so a subsequent reboot goes into normal client mode).

---

## Captive Portal Mechanics

### How mobile devices detect captive portals

When a device connects to a new WiFi network, the OS makes probe requests
to well-known URLs:

| OS | Probe URL | Expected response |
|----|-----------|-------------------|
| Android | `http://connectivitycheck.gstatic.com/generate_204` | HTTP 204 No Content |
| Android (alt) | `http://clients3.google.com/generate_204` | HTTP 204 No Content |
| iOS | `http://captive.apple.com/hotspot-detect.html` | Specific HTML body |
| Windows | `http://www.msftconnecttest.com/ncsi.txt` | Text `"Microsoft NCSI"` |

When our gateway **doesn't** return the expected response (because dnsmasq
resolves these domains to `192.168.100.1` and we redirect to `/setup`), the
OS detects a captive portal and pops up a browser window automatically.

The gateway in setup mode handles these probe URLs specifically:
- `GET /generate_204` → returns HTTP **200 with redirect** to `/setup`
  (not 204 — that would tell the OS the portal is already authenticated)
- All other GET requests → redirect to `http://192.168.100.1/setup`

### Why iptables redirect is needed

Without iptables, clients browsing to `http://google.com` would get a DNS
response of `192.168.100.1` (from dnsmasq wildcard), but the HTTP Host header
would say `google.com`. Our web server handles this by ignoring the Host header
and always serving the setup page.

The iptables DNAT ensures even HTTPS attempts on port 443 get redirected to
our HTTP setup page (the connection won't validate TLS — the browser shows a
certificate error briefly, then the OS captive portal handler takes over).

---

## Potential Issues

### Issue 1: rmmod/insmod during AP fallback

The AP fallback path does one driver reload (CLIENT → AP). This is the same
reload that `wifi-setup.sh` already does at every boot (AP → CLIENT). So the
total reload count per boot session is always exactly 1 — well within the
pronto driver's safety margin.

The watchdog does not run in setup mode, so there is no race condition with
the 30-second auto-reboot trigger.

### Issue 2: hostapd/dnsmasq may already be running

Android init starts `hostapd` and `dnsmasq` as system services. `wifi-ap-start.sh`
kills any existing instances before starting its own. Since we're doing a
driver reload anyway, `hostapd` (which is a kernel thread on this device when
the wlan module is loaded) is killed by `rmmod wlan`.

### Issue 3: rndis0 USB access during AP mode

The USB RNDIS interface (`rndis0`, `192.168.100.1`) continues to work in AP
mode. A user with a USB cable and `sudo ip addr add 192.168.100.2/24 dev
enxXXXX` on their PC can reach the setup page at `http://192.168.100.1/` even
without connecting to the WiFi hotspot. Useful for recovery if the hotspot
doesn't work.

### Issue 4: 120s timeout false positives

If the router is slow (rebooting, firmware update), `wifi-setup.sh` may time
out even though the credentials are correct. The device falls into AP mode
unnecessarily. The user connects to `SMS-Gateway-Setup`, sees the saved
networks are correct, and can hit "Save & Reboot" without changing anything.
The second boot succeeds. This is acceptable behaviour.

### Issue 5: No internet in AP mode

The dongle in AP mode has 4G data (modem is always running independently of
WiFi), but client devices connected to `SMS-Gateway-Setup` will not get
internet access — only the setup page. This is intentional and expected for
a captive portal setup flow.

---

## Implementation Plan

### Phase 1: `sms-gateway --setup-mode` (core)

| Step | Task | Status |
|------|------|--------|
| 1.1 | Add `--setup-mode` flag to `main.go`; skip AT session + all goroutines except web server | ✅ Done |
| 1.2 | Add setup-mode handler in `cmd/sms-gateway/setup_mode.go` (separate from main server) | ✅ Done |
| 1.3 | Write inline setup HTML — WiFi network list, add form, Save & Reboot button | ✅ Done |
| 1.4 | Implement `/setup/add`, `/setup/delete`, `/setup/save` — validate, write config + wpa conf, reboot | ✅ Done |
| 1.5 | Add captive portal probe handlers (`/generate_204`, `/hotspot-detect.html`, `/ncsi.txt`) | ✅ Done |
| 1.6 | Add `force_ap_mode` to `WiFiConfig` struct | ✅ Done |

### Phase 2: AP mode infrastructure

| Step | Task | Status |
|------|------|--------|
| 2.1 | Write `wifi-ap-start.sh` (driver reload, hostapd, dnsmasq, iptables) | ✅ Done |
| 2.2 | Write `hostapd-setup.conf` with `SMS-Gateway-Setup` SSID + fixed password | ✅ Done |
| 2.3 | Remove hardcoded creds from `wifi-setup.sh`; use `/data/misc/wifi/wpa_supplicant.conf` | ✅ Done |
| 2.4 | Extract `config.WriteWPAConf()` shared function in `internal/config/wpa.go` | ✅ Done |
| 2.5 | Add AP fallback branch to `start.sh` (force_ap_mode check + post-attempt IP check) | ✅ Done |

### Phase 3: Integration

| Step | Task | Status |
|------|------|--------|
| 3.1 | End-to-end test with `force_ap_mode: true` (Level 1) | ✅ Done — AP hotspot visible, captive portal works, save + reboot cycle verified |
| 3.2 | Test wrong-password path (real failure) | ⬜ Not yet done |
| 3.3 | Test captive portal on iOS, Android, desktop | ⬜ Not yet done |
| 3.4 | Test that normal SMS operation resumes after AP → reboot → client | ✅ Done (normal boot after Level 1 test) |
| 3.5 | Deploy and update docs | ✅ Done 2026-04-11 |
| 3.6 | WiFi management UI in Settings page (bonus — add/edit/reorder/remove) | ✅ Done 2026-04-11 |

---

## Testing Strategy

The primary challenge: the `NETGEAR_24ng` network is always visible during
development, so the device will always connect successfully and never trigger
AP fallback naturally. The following approaches test each part of the flow
independently before requiring a real "no known WiFi" environment.

### Level 1 — Force AP mode flag (safest, most repeatable)

Set `"force_ap_mode": true` in `/data/sms-gateway/config.json`:

```bash
# On device via adb (requires root):
adb shell "/system/xbin/librank /system/bin/sh -c \
  'cat /data/sms-gateway/config.json | \
   busybox sed \"s/\\\"wifi\\\": {/\\\"wifi\\\": {\\n    \\\"force_ap_mode\\\": true,/\" \
   > /data/sms-gateway/config.json.new && \
   mv /data/sms-gateway/config.json.new /data/sms-gateway/config.json'"

# Easier: edit on host, push
# Edit config.json locally, add "force_ap_mode": true to wifi section
adb push config.json /data/sms-gateway/config.json
adb reboot
```

**What this tests**:
- AP mode starts correctly (`SMS-Gateway-Setup` hotspot visible)
- Captive portal works (connect phone, browser opens setup page automatically)
- Setup page renders correctly with existing saved networks
- "Add Network" form saves to config correctly
- "Save & Reboot" triggers reboot (flag is cleared by the save handler)
- Device boots into client mode on next reboot

**Restore normal operation**: The save handler clears `force_ap_mode` before
rebooting, so the device returns to normal automatically after a successful
save + reboot cycle.

### Level 2 — Wrong password test (tests the real timeout path)

Edit `wpa_supplicant.conf` on the device to use a wrong password for
`NETGEAR_24ng`, then reboot:

```bash
# Save current config
adb pull /data/misc/wifi/wpa_supplicant.conf wpa_supplicant.conf.bak

# Edit: change NETGEAR_24ng psk to something wrong
# Push modified version
adb push wpa_supplicant_wrong.conf /data/misc/wifi/wpa_supplicant.conf
adb reboot
```

**What this tests**:
- `wifi-setup.sh` times out after 120s when credentials are wrong
- `start.sh` correctly detects the failure and falls back to AP mode
- Full end-to-end flow including the actual 120s wait

**Restore**: After test, user adds NETGEAR_24ng back via the setup page (or
restore from backup via adb).

**Note**: This test takes ~2 minutes for the timeout to expire on each boot.
Use Level 1 for iterating on the UI; use Level 2 only for validating the
full timeout path.

### Level 3 — Remove all saved networks (tests missing config path)

Edit `config.json` so `wifi.networks` is an empty array. `wifi-setup.sh`
generates a `wpa_supplicant.conf` with no networks, wpa_supplicant fails to
associate immediately, and the timeout fires quickly (no 120s wait — it fails
fast when there are no configured networks).

**What this tests**: The "no WiFi config at all" first-boot scenario.

### Level 4 — Component testing (no reboots needed)

Test individual pieces without rebooting by running them manually via adb:

```bash
# Test AP mode infrastructure only (no sms-gateway involved)
adb shell "/system/xbin/librank sh /data/sms-gateway/scripts/wifi-ap-start.sh"
# → Verify: SMS-Gateway-Setup hotspot appears on phone
# → Verify: phone gets 192.168.100.x IP from dnsmasq
# → Verify: browser on phone gets redirected to setup page

# Test setup-mode gateway only (AP already running from above)
adb shell "/system/xbin/librank /data/sms-gateway/sms-gateway \
  --config /data/sms-gateway/config.json --setup-mode &"
# → Verify: http://192.168.100.1/ shows setup page
# → Verify: captive portal probes work (curl tests from phone)

# Restore: kill setup-mode gateway, run wifi-setup.sh normally
adb shell "/system/xbin/librank sh /data/sms-gateway/scripts/wifi-setup.sh"
```

### Level 5 — Pre-deployment validation at unknown location

Before deploying to Marlow FM (or any new location), the full flow must be
validated with no known network available. Use a phone hotspot that is NOT
in the saved networks:

1. Turn off `NETGEAR_24ng` router (or move dongle away from it)
2. Ensure only an unknown hotspot is in range
3. Reboot dongle
4. Confirm `SMS-Gateway-Setup` appears within 3 minutes
5. Connect phone to `SMS-Gateway-Setup` (password: `smsgateway`)
6. Confirm captive portal page opens automatically
7. Add the test hotspot credentials
8. Click "Save & Reboot"
9. Confirm dongle connects to test hotspot and web UI is accessible
10. Confirm SMS→email flow works (send test text, verify email received)

### Test Matrix

| Scenario | Level | Expected outcome |
|----------|-------|-----------------|
| `force_ap_mode: true`, reboot | 1 | AP hotspot up, captive portal works |
| Save new network via setup page | 1 | Config written, device reboots, connects |
| Wrong password for saved network | 2 | 120s timeout, AP fallback, setup page |
| No saved networks at all | 3 | Immediate failure, AP fallback |
| iOS captive portal detection | 4 | Setup page opens automatically in Safari |
| Android captive portal detection | 4 | Setup page opens in system browser |
| Windows captive portal detection | 4 | Notification bar → setup page |
| SMS flow after AP → reboot → client | 4/5 | Text received, forwarded to email |
| ADB USB access during AP mode | 4 | Setup page at 192.168.100.1 via RNDIS |

---

## File Changes

### New Files
```
sms-gateway/
├── cmd/sms-gateway/setup_mode.go    # --setup-mode: web server only, no modem
├── internal/web/
│   └── templates/setup.html         # WiFi setup page template
└── scripts/
    ├── wifi-ap-start.sh              # AP mode network setup
    └── hostapd-setup.conf            # Static hostapd config for setup SSID
```

### Modified Files
```
sms-gateway/
├── cmd/sms-gateway/main.go          # Add --setup-mode flag, skip goroutines
├── internal/config/config.go        # Add ForceAPMode bool to WiFiConfig
├── internal/web/server.go           # Add /setup, /generate_204 etc. routes
└── scripts/
    ├── start.sh                      # Add 120s timeout, AP fallback branch
    └── wifi-setup.sh                 # Read wifi.networks from config.json
```

### What Does NOT Change
- The `sms-gateway` normal-mode flow — identical to current
- AT command session, SMS poller, IMAP IDLE — unchanged
- SQLite schema — unchanged
- Existing web UI templates — unchanged
- The WiFi watchdog auto-reboot — still runs in normal mode; not started in setup mode

---

## Success Criteria

1. **Boot with valid WiFi config** → dongle connects to WiFi, gateway starts,
   web UI available at DHCP-assigned IP within 2 minutes.
2. **Boot with invalid/missing WiFi config** → dongle creates `SMS-Gateway-Setup`
   hotspot within 3 minutes. User connects phone, browser opens setup page.
3. **User saves new SSID/PSK** → dongle reboots, connects to new network,
   SMS flow resumes. `force_ap_mode` is cleared automatically.
4. **Network drops while in CLIENT mode** → existing WiFi watchdog handles it
   (soft reconnect → auto-reboot if driver crashes). AP fallback is not involved.
5. **force_ap_mode test** → matches Level 1 test matrix exactly.

---

*See also: `STATUS.md` (current status), `GATEWAY.md` (architecture),
`BUGS.md` (WiFi driver issues — Bugs 15–19), `DEVICE.md` (hardware specs,
WiFi mode switch procedure)*
