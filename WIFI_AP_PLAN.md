# WiFi AP Fallback — Design Plan
## Auto-Healing Connectivity for the ZTE UFI103 SMS Gateway

*Created: 2026-04-05*
*Trigger: When the dongle boots and can't connect to its saved WiFi network,
it falls back to AP mode so a user can configure WiFi credentials via a web page.*

**Cross-reference**: This plan implements the "boot persistence" gap described
in `STATUS.md` Known Issues #3 and `BUGS.md` "Boot Persistence Issue".
Once implemented, those sections should be updated.

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

**Current behaviour**: After boot, the dongle always starts in AP mode (hotspot)
because Android init launches `hostapd`. The user must run
`wifi-client-start.sh` (via ADB) to switch to WiFi client mode.

**Desired behaviour**: The dongle automatically tries WiFi client mode on boot.
If it can't connect (wrong credentials, no network), it falls back to AP mode
and serves a WiFi setup page. The user connects their phone/laptop to the
dongle's hotspot, opens a browser, and configures the WiFi credentials.

---

## Architecture

### Components

```
┌─────────────────────────────────────────────────────────────┐
│  /data/sms-gateway/wifi-manager (Go binary, ~10MB)          │
│                                                             │
│  State machine:                                             │
│  1. BOOT → read config, try WiFi client mode                │
│  2. CLIENT → wpa_supplicant + DHCP client                   │
│     ├─ success → start sms-gateway, serve normal web UI     │
│     └─ timeout (90s) → go to AP mode                        │
│  3. AP MODE → hostapd + dnsmasq + iptables redirect         │
│     ├─ serve WiFi setup page at all HTTP URLs               │
│     ├─ user submits SSID + PSK → save config               │
│     └─ switch to CLIENT mode (driver reload)                │
│                                                             │
│  The existing sms-gateway binary serves the web UI.          │
│  wifi-manager controls mode switching, WiFi state, and       │
│  optionally embeds the setup HTML page.                      │
└─────────────────────────────────────────────────────────────┘
```

### State Machine

```
BOOT
  │
  ├─ Load config.json → read saved SSID/PSK
  │
  ├─ Is WiFi config present and valid?
  │   ├─ NO  → go to AP MODE immediately
  │   └─ YES → continue
  │
  ├─ Switch to CLIENT mode (reload driver)
  │   ├─ Write wpa_supplicant.conf
  │   ├─ Start wpa_supplicant
  │   ├─ Start DHCP client (udhcpc)
  │   └─ Wait up to 90 seconds
  │
  ├─ Did association + DHCP succeed?
  │   ├─ YES → Start sms-gateway, serve normal web UI → stay in CLIENT
  │   └─ NO  → Switch to AP MODE
  │
  └─ AP MODE
      ├─ Start hostapd (from existing /data/misc/wifi/hostapd.conf)
      ├─ Ensure dnsmasq is running (serves 192.168.100.x)
      ├─ Set up iptables redirect: all HTTP → 192.168.100.1:8080
      ├─ Start sms-gateway in "setup mode" (serves WiFi config page)
      ├─ User submits form → save SSID/PSK to config.json
      ├─ Show "Reconnecting..." page
      └─ Go to CLIENT mode (driver reload with new config)
```

### Mode Switching Protocol

The core challenge: the WiFi driver (`pronto_wlan.ko`) can only operate in
**one mode at a time**. Switching requires:

```bash
# CLIENT → AP mode
kill wpa_supplicant
brctl delif bridge1 wlan0 2>/dev/null
ifconfig wlan0 down
rmmod wlan
sleep 3
insmod /system/lib/modules/pronto/pronto_wlan.ko
sleep 5
ifconfig wlan0 up
brctl addif bridge1 wlan0
brctl addif bridge1 rndis0
ifconfig bridge1 192.168.100.1 netmask 255.255.255.0 up
ifconfig rndis0 192.168.100.1 netmask 255.255.255.0 up
# hostapd auto-starts via Android init or start manually
dnsmasq --keep-in-foreground --dhcp-range=192.168.100.2,192.168.100.254,3h

# AP → CLIENT mode
kill dnsmasq (or let it continue for rndis0)
brctl delif bridge1 wlan0
brctl delif bridge1 rndis0 2>/dev/null
ifconfig bridge1 down
ifconfig wlan0 down
rmmod wlan
sleep 3
insmod /system/lib/modules/pronto/pronto_wlan.ko
sleep 5
ifconfig wlan0 up
# Start wpa_supplicant with new config
# Start udhcpc
```

---

## Detailed Design

### 1. The WiFi Manager Binary

A new Go binary (`wifi-manager`) that:

- Is the **only** entry point on boot (replaces `start.sh`)
- Manages the state machine described above
- Launches `sms-gateway` as a subprocess (not a separate service)
- Embeds the WiFi setup HTML page as a Go `embed.FS`
- Listens on `:8080` in all modes — the handler changes based on state

**Why not extend sms-gateway?** Keeping mode-switching logic separate from the
gateway means the gateway binary stays focused on SMS/email bridging. The
wifi-manager can kill/restart the gateway when switching modes.

**Why a subprocess?** The existing sms-gateway opens `/dev/smd11` with a
persistent reader. If wifi-manager also tries to use the AT commands for
WiFi setup, there would be contention. The wifi-manager owns the AT session;
the gateway is started only in CLIENT mode after WiFi is connected.

### 2. WiFi Setup Web Page

**Served at**: ALL HTTP URLs (captive portal via iptables redirect)

**Content**:
```
┌─────────────────────────────────────────┐
│  🔌 4G-UFI-DD78 WiFi Setup              │
├─────────────────────────────────────────┤
│                                         │
│  WiFi Network (SSID):                   │
│  ┌──────────────────────────────────┐   │
│  │ YOUR_WIFI_SSID_1                     │   │
│  └──────────────────────────────────┘   │
│                                         │
│  WiFi Password:                         │
│  ┌──────────────────────────────────┐   │
│  │ ••••••••                         │   │
│  └──────────────────────────────────┘   │
│                                         │
│  [ Save & Connect ]                     │
│                                         │
│  ─── Advanced ───                       │
│                                         │
│  AP Name: 4G-UFI-DD78                   │
│  AP Password: 1234567890                │
│  Email Server: [IP/hostname]            │
│  Email Port: [993/587]                  │
│  Email User: [username]                 │
│  Email Pass: [password]                 │
│  Forward To: [email]                    │
│  SIM PIN: [8837] (or leave blank)       │
│                                         │
│  [ Save All Settings ]                  │
└─────────────────────────────────────────┘
```

**Behaviour on submit**:
1. Validate SSID (non-empty, ≤32 chars) and PSK (≥8 chars)
2. Write `/data/sms-gateway/config.json` with new WiFi credentials
3. Write `/data/misc/wifi/wpa_supplicant.conf`
4. Return "Reconnecting..." page with meta-refresh every 5s
5. Kill gateway → switch to CLIENT mode → restart gateway

### 3. Captive Portal (iptables redirect)

When in AP mode, all HTTP traffic from connected clients is redirected to
the setup page:

```bash
# Redirect all port 80 traffic to the gateway's web server
iptables -t nat -A PREROUTING -i wlan0 -p tcp --dport 80 -j DNAT \
    --to-destination 192.168.100.1:8080
iptables -t nat -A PREROUTING -i wlan0 -p tcp --dport 443 -j DNAT \
    --to-destination 192.168.100.1:8080

# Allow the gateway's own traffic
iptables -t nat -A POSTROUTING -o rndis0 -j MASQUERADE
```

**DNS handling**: dnsmasq resolves all domains to `192.168.100.1`. This
triggers the captive portal detection on iOS/Android/Windows.

```bash
dnsmasq \
    --interface=bridge1 \
    --dhcp-range=192.168.100.2,192.168.100.254,3h \
    --address=/#/192.168.100.1 \
    --no-resolv \
    --no-hosts
```

**How clients discover the page**:
- **iOS/Android**: Detect captive portal by fetching a known URL (e.g.,
  `captive.apple.com`). Gets redirected to `192.168.100.1:8080`, opens
  the setup page automatically.
- **Desktop browsers**: User types any URL → gets redirected → sees setup.
- **Manual**: `http://192.168.100.1:8080/` always works.

### 4. Configuration Storage

The existing `config.json` is extended:

```json
{
    "wifi": {
        "mode": "client",
        "client": {
            "ssid": "YOUR_WIFI_SSID_1",
            "psk": "YOUR_WIFI_PASSWORD"
        },
        "ap": {
            "ssid": "4G-UFI-DD78",
            "psk": "1234567890"
        }
    },
    "sms": { ... },
    "email": { ... },
    "authorised_senders": ["your-email@example.com"],
    "database": "/data/sms-gateway/sms.db",
    "log_file": "/data/sms-gateway/sms-gateway.log",
    "web": { "listen_addr": "0.0.0.0:8080" }
}
```

**New fields** (with defaults):
- `wifi.mode` — `"client"` or `"ap"` (auto-set by wifi-manager)
- `wifi.client.ssid` — saved WiFi network
- `wifi.client.psk` — saved WiFi password
- `wifi.ap.ssid` — hotspot name (derived from MAC: `4G-UFI-XXXX`)
- `wifi.ap.psk` — hotspot password (default: `1234567890`)

### 5. Boot Flow

The `start.sh` script is replaced by `wifi-manager`:

```
/data/local/userinit.sh (if it works) or debuggerd wrapper
  → /data/sms-gateway/scripts/wifi-manager-start.sh
    → /data/sms-gateway/wifi-manager --config /data/sms-gateway/config.json
      → wifi-manager decides: CLIENT or AP mode
        → CLIENT: start sms-gateway subprocess, serve normal UI
        → AP: serve WiFi setup page, wait for user input
```

**wifi-manager-start.sh**:
```bash
#!/system/bin/sh
# Waits for modem, rotates logs, launches wifi-manager
sleep 30
exec /data/sms-gateway/wifi-manager --config /data/sms-gateway/config.json \
    >> /data/sms-gateway/sms-gateway.log 2>&1
```

---

## Potential Issues & Challenges

### Issue 1: hostapd is an Android Service

**Problem**: Android init launches `hostapd` automatically via `init.qcom.rc`.
It's also a kernel thread `[hostapd]` that cannot be killed — only `rmmod wlan`
kills it.

**Impact**: In CLIENT mode, Android's hostapd is still "running" as a dead
kernel thread. When we reload the driver in AP mode, Android init may not
restart it automatically.

**Mitigation**: wifi-manager manually starts `hostapd` in AP mode:
```bash
/system/bin/hostapd -e /data/misc/wifi/entropy.bin \
    /data/misc/wifi/hostapd.conf -B
```

### Issue 2: dnsmasq is Also an Android Service

**Problem**: `dnsmasq` runs as a system service for the AP's DHCP. If we kill
it in CLIENT mode, it won't auto-restart in AP mode.

**Impact**: AP mode won't hand out DHCP addresses to clients.

**Mitigation**: wifi-manager manually manages dnsmasq:
- CLIENT mode: kill dnsmasq (not needed — udhcpc handles DHCP client)
- AP mode: start dnsmasq manually with explicit args

### Issue 3: Driver Reload Takes 10-15 Seconds

**Problem**: `rmmod wlan` + `insmod pronto_wlan.ko` + firmware load takes
~10 seconds. During this time, no WiFi at all.

**Impact**: User experience gap when switching modes.

**Mitigation**: The "Reconnecting..." page with auto-refresh handles this
gracefully. In practice, the switch happens rarely (only after config change
or boot failure).

### Issue 4: SIM PIN on Boot

**Problem**: Now that we've removed the SIM PIN lock (`AT+CLCK="SC",0,"8837"`),
this is no longer an issue. But if the SIM is re-locked (rare), the gateway
needs to unlock it.

**Status**: Resolved. The SIM PIN is permanently disabled.

### Issue 5: AP SSID Uniqueness

**Problem**: The default AP name is `4G-UFI-` + last 4 chars of MAC. If the
user moves to an environment where another device uses the same name, there's
a conflict.

**Mitigation**: The setup page allows the user to change the AP SSID. The
default is auto-generated from the MAC address.

### Issue 6: Bridge Configuration

**Problem**: In AP mode, `wlan0` and `rndis0` must be bridged for USB tethering
to work while also serving the hotspot. The bridge (`bridge1`) must have an IP.

**Mitigation**:
```bash
brctl addbr bridge1 2>/dev/null
brctl addif bridge1 wlan0
brctl addif bridge1 rndis0
ifconfig bridge1 192.168.100.1 netmask 255.255.255.0 up
```
The gateway web server listens on `0.0.0.0:8080`, so it's reachable via
`bridge1` (AP hotspot) and `rndis0` (USB) simultaneously.

### Issue 7: iptables State Persistence

**Problem**: iptables rules are not persisted across reboots. If the device
crashes while in AP mode with captive portal rules, it reboots into the
default state.

**Mitigation**: wifi-manager sets up iptables rules at startup based on the
current mode. Rules are always transient — wifi-manager manages them.

### Issue 8: No DNS Resolution in AP Mode

**Problem**: In AP mode, the dongle has no internet, so DNS queries from
clients will fail. Captive portal detection on mobile devices relies on DNS
redirecting to a local page.

**Mitigation**: dnsmasq `--address=/#/192.168.100.1` resolves ALL domains to
the local gateway. Combined with iptables port redirect, any HTTP request
lands on the setup page.

### Issue 9: Config Validation

**Problem**: User might enter invalid SSID/PSK or break the email config via
the setup page.

**Mitigation**:
- Client-side JS validates form fields before submission
- Server-side validation rejects invalid configs
- Previous working config is kept as fallback
- If new config causes connection failure, the device falls back to AP mode
  automatically on next boot

### Issue 10: Memory Budget

**Problem**: The device has 512MB RAM. Running hostapd + dnsmasq + wifi-manager
+ sms-gateway + Android framework simultaneously may exceed budget.

**Analysis**:
- Android framework + zygote: ~200MB
- hostapd: ~5MB
- dnsmasq: ~2MB
- wifi-manager: ~10MB
- sms-gateway: ~15MB
- Total: ~232MB → well within 512MB budget

**Status**: Safe. The device runs all of these in CLIENT mode already (Android
keeps hostapd as a kernel thread).

---

## Implementation Plan

### Phase 1: WiFi Manager Core (Week 1)

**Goal**: wifi-manager binary boots, manages mode switching, starts gateway.

| Step | Task | Details |
|------|------|---------|
| 1.1 | New Go module | `internal/wifimgr/` package |
| 1.2 | Config extension | Add `WiFiConfig` struct to `config/config.go` |
| 1.3 | Mode detector | Read config, check for valid SSID/PSK |
| 1.4 | CLIENT switch | Shell out to `wifi-client-start.sh` logic |
| 1.5 | AP switch | Shell out to new `wifi-ap-start.sh` logic |
| 1.6 | DHCP wait | Poll `udhcpc` result with 90s timeout |
| 1.7 | Gateway launcher | Start/stop sms-gateway as subprocess |

### Phase 2: AP Mode Infrastructure (Week 2)

**Goal**: AP mode works — clients can connect, get DHCP, reach setup page.

| Step | Task | Details |
|------|------|---------|
| 2.1 | `wifi-ap-start.sh` | Reverse of client script |
| 2.2 | hostapd launch | Manual `hostapd -B` with generated config |
| 2.3 | dnsmasq launch | Manual with `--address=/#/192.168.100.1` |
| 2.4 | iptables rules | NAT redirect for captive portal |
| 2.5 | Bridge setup | bridge1 with wlan0 + rndis0 |
| 2.6 | AP SSID generation | `4G-UFI-` + last 4 of MAC |

### Phase 3: Web Setup UI (Week 2)

**Goal**: User can configure WiFi credentials via browser.

| Step | Task | Details |
|------|------|---------|
| 3.1 | Setup HTML page | Embedded via Go `embed.FS` |
| 3.2 | Config API endpoint | POST `/api/wifi-config` handler |
| 3.3 | Server-side validation | Reject empty/bad SSID/PSK |
| 3.4 | wpa_supplicant.conf write | On config save |
| 3.5 | Reconnect page | "Switching to client mode..." with auto-refresh |
| 3.6 | Advanced settings page | Email, AP name, authorised senders |

### Phase 4: Integration & Testing (Week 3)

**Goal**: End-to-end flow works on device.

| Step | Task | Details |
|------|------|---------|
| 4.1 | Boot flow integration | Replace start.sh, update userinit.sh |
| 4.2 | CLIENT → AP fallback | Test with wrong SSID |
| 4.3 | AP → CLIENT switch | Test saving new credentials |
| 4.4 | Captive portal detection | Test on iOS/Android/desktop |
| 4.5 | SMS gateway in CLIENT | Verify SMS flow after reconnect |
| 4.6 | Error handling | Bad config, driver failure, SMSC issues |

---

## File Changes

### New Files
```
sms-gateway/
├── cmd/wifi-manager/
│   └── main.go              # WiFi manager entry point
├── internal/wifimgr/
│   ├── manager.go           # State machine, mode switching
│   ├── client_mode.go       # CLIENT mode setup (wpa_supplicant + DHCP)
│   ├── ap_mode.go           # AP mode setup (hostapd + dnsmasq + iptables)
│   ├── captive.go           # Captive portal web handler + embedded HTML
│   └── subprocess.go        # Launch/stop sms-gateway
└── scripts/
    ├── wifi-manager-start.sh # Boot wrapper (replaces start.sh)
    └── wifi-ap-start.sh     # AP mode network setup
```

### Modified Files
```
sms-gateway/
├── internal/config/config.go  # Add WiFiConfig struct
├── scripts/start.sh           # Replace: call wifi-manager instead
sms-gateway/go.mod             # No new deps (uses stdlib for exec/shell)
```

### Device Files (pushed by deploy)
```
/data/sms-gateway/
├── wifi-manager               # New binary
├── scripts/wifi-manager-start.sh  # New boot script
├── config.json                # Extended with wifi section
/data/misc/wifi/
├── wpa_supplicant.conf        # Written by setup page
├── hostapd.conf               # Modified with dynamic SSID/PSK
/system/bin/debuggerd          # Wrapper (already in place)
```

---

## What This Does NOT Change

- **The sms-gateway binary** — unchanged, still handles SMS/email bridging
- **The AT command session** — unchanged, still persistent reader on /dev/smd11
- **The SQLite database** — unchanged schema
- **The IMAP IDLE implementation** — unchanged, still persistent connection
- **The SIM PIN lock** — already permanently removed (not a config option)
- **The existing web UI templates** — unchanged; setup page is a separate route

---

## Success Criteria

1. **Boot with valid WiFi config** → dongle connects to WiFi, gateway starts,
   web UI available at DHCP-assigned IP within 60 seconds.
2. **Boot with invalid/missing WiFi config** → dongle creates AP hotspot within
   45 seconds. User connects phone, opens browser, is redirected to setup page.
3. **User saves new SSID/PSK** → dongle switches to CLIENT mode, connects,
   gateway starts, SMS flow resumes.
4. **Network drops while in CLIENT mode** → gateway keeps running (IMAP IDLE
   reconnects automatically). If modem dies, circuit breaker backs off.
5. **User changes WiFi password on router** → next boot fails → dongle falls
   back to AP mode → user updates credentials → reconnects.

---

*See also: `STATUS.md` (current status), `GATEWAY.md` (architecture),
`BUGS.md` (known issues), `REFACTOR_PLAN.md` (completed refactoring items),
`DEVICE.md` (hardware specs, WiFi mode switch procedure),
`WIFI_AP_TEST_PLAN.md` (WiFi AP test plan),
`FULL_PROJECT_TEST_PLAN.md` (comprehensive test plan — 103 tests passing),
`DOCUMENTATION_PLAN.md` (documentation roadmap)*
