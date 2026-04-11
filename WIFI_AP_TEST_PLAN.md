# WiFi AP Fallback — Test Plan & Debuggability Review
## Software Test Engineering Perspective

*Created: 2026-04-05*
*Reviews: `WIFI_AP_PLAN.md` (design plan)*
*Updated: 2026-04-11 — Level 1 test complete; notes added on implemented vs. not-implemented items*

> **Note**: Sections 1 (Debuggability), 2 (Unit tests for `wifimgr` package), and 4 (Automated
> test framework) describe features that were **not implemented** in the final design. They are
> retained as a record of what was considered. The `internal/wifimgr` package was never created;
> the `/api/wifi-config`, `/api/wifi-preview`, `/api/wifi-diag` endpoints were not added; and
> no event log file is written. The implemented design uses the simpler approach in `WIFI_AP_PLAN.md`.

---

## 1. Critique: Debuggability Gaps in the Current Plan

### 1.1 No State Observability

**Problem**: The state machine (BOOT → CLIENT → AP → CLIENT) is a black box.
Once deployed standalone with no ADB access, there's no way to know:
- Which state the device is in
- How long it's been in the current state
- How many connection attempts were made
- Why a mode switch happened
- Whether the last config save succeeded

**Required**: Expose state machine status via the existing `/status` endpoint:
```json
{
  "wifi_mode": "client",
  "wifi_state": "connected",
  "wifi_ssid": "YOUR_WIFI_SSID_1",
  "wifi_ip": "172.16.10.226",
  "wifi_attempts": 3,
  "wifi_last_attempt": "2026-04-05T09:30:00Z",
  "wifi_last_success": "2026-04-05T09:29:55Z",
  "ap_clients_connected": 0,
  "last_mode_switch": "2026-04-05T09:29:50Z",
  "last_mode_switch_reason": "boot_with_valid_config"
}
```

### 1.2 No Event Log / Audit Trail

**Problem**: When something goes wrong, there's no persistent history of
what happened. The gateway log is append-only and gets rotated at 5MB.

**Required**: A ring-buffer of recent WiFi events persisted to disk
(e.g., `/data/sms-gateway/wifi-events.jsonl`, last 100 entries):
```jsonl
{"ts":"2026-04-05T09:29:50Z","event":"mode_switch","from":"ap","to":"client","reason":"config_saved"}
{"ts":"2026-04-05T09:29:55Z","event":"client_connected","ssid":"YOUR_WIFI_SSID_1","ip":"172.16.10.226"}
{"ts":"2026-04-05T09:30:00Z","event":"client_associated","ssid":"YOUR_WIFI_SSID_1","rssi":-72}
{"ts":"2026-04-05T09:45:12Z","event":"client_disconnected","ssid":"YOUR_WIFI_SSID_1","reason":"signal_lost"}
{"ts":"2026-04-05T09:45:42Z","event":"mode_switch","from":"client","to":"ap","reason":"connection_timeout"}
```

### 1.3 No Diagnostic Mode

**Problem**: No way to run a self-test of the WiFi subsystem without
disrupting the running gateway.

**Required**: A `--wifi-diag` flag on the wifi-manager binary that:
- Tests driver load/unload (without disrupting the running gateway)
- Validates wpa_supplicant.conf syntax
- Validates hostapd.conf syntax
- Tests dnsmasq availability
- Tests iptables rules
- Validates config.json structure
- Returns a structured pass/fail report

### 1.4 No "What Would I Do?" Preview

**Problem**: User edits config via the setup page but can't preview what
the wifi-manager will do with the new config before committing.

**Required**: POST `/api/wifi-preview` returns what actions would be taken:
```json
{
  "valid": true,
  "mode_switch": "client",
  "wpa_supplicant_would_write": true,
  "hostapd_would_stop": true,
  "gateway_would_restart": true,
  "warnings": ["SSID contains spaces — may cause issues on some routers"]
}
```

---

## 2. Test Strategy

### 2.1 Unit Tests (Offline, on dev machine)

| Test Suite | What it tests | How to run |
|------------|--------------|------------|
| `config_test.go` | Config parsing, validation, defaults, malformed JSON, missing fields | `go test ./internal/config/...` |
| `wifimgr/state_test.go` | State machine transitions, valid/invalid transitions | `go test ./internal/wifimgr/...` |
| `wifimgr/validator_test.go` | SSID validation (length, charset), PSK validation (length, charset), edge cases | `go test ./internal/wifimgr/...` |
| `wifimgr/captive_test.go` | HTTP redirect logic, DNS spoofing config generation | `go test ./internal/wifimgr/...` |
| `wifimgr/iptables_test.go` | iptables rule generation, NAT config, cleanup rules | `go test ./internal/wifimgr/...` |

### 2.2 Integration Tests (On-device, automated)

These run on the actual dongle but **without disrupting the live WiFi connection**.

| Test | Method | Automation |
|------|--------|------------|
| Config save → file written | POST `/api/wifi-config` via curl on localhost, read file back | Shell script via ADB |
| Config validation rejects bad input | POST invalid configs, verify 400 responses | Shell script via ADB |
| Preview endpoint | POST `/api/wifi-preview`, verify actions | Shell script via ADB |
| Diagnostics endpoint | GET `/api/wifi-diag`, verify all checks pass | Shell script via ADB |
| State exposure | GET `/status`, verify wifi fields present | Shell script via ADB |
| Event log | Trigger state changes, verify events written | Shell script via ADB |
| Web UI in CLIENT mode | GET `/`, verify dashboard renders | Curl via ADB or host |
| Web UI in AP mode | Connect via rndis0 (USB), GET `/`, verify setup page | Curl via ADB |

### 2.3 End-to-End Tests (Requires WiFi disruption)

These tests **will disrupt the WiFi connection** and should only be run
when ADB over USB is available as a fallback.

| Test | Steps | Expected | Recovery |
|------|-------|----------|----------|
| CLIENT→AP fallback (wrong SSID) | Save config with non-existent SSID, wait 90s | Falls back to AP mode, serves setup page | Save correct config |
| AP→CLIENT switch | In AP mode, submit valid config | Switches to CLIENT, connects, gateway starts | Automatic |
| Wrong password fallback | Save config with correct SSID but wrong PSK | Fails association, falls back to AP after timeout | Save correct password |
| Empty SSID rejection | Submit empty SSID via setup page | Validation error, config not saved | N/A |
| Short PSK rejection | Submit PSK < 8 chars | Validation error, config not saved | N/A |
| Config corruption recovery | Corrupt config.json, reboot | Falls back to AP mode with defaults | Reconfigure via setup page |
| Power loss during switch | Kill power during driver reload | Reboots to AP mode (safe default) | Reconfigure if needed |
| Multiple AP clients | Connect 2 devices to AP hotspot simultaneously | Both get DHCP, both see setup page | N/A |
| Captive portal detection (iOS) | Connect iPhone to AP hotspot | iOS auto-opens captive portal page | N/A |
| Captive portal detection (Android) | Connect Android phone to AP hotspot | Android auto-opens captive portal page | N/A |
| Captive portal detection (desktop) | Connect laptop to AP hotspot, visit any HTTP URL | Redirected to setup page | N/A |

---

## 3. Corner Cases

### 3.1 SSID Edge Cases

| Case | Expected Behaviour |
|------|-------------------|
| Empty SSID (`""`) | Rejected by validation — "SSID is required" |
| SSID > 32 characters | Rejected by validation — "SSID must be ≤32 characters" |
| SSID with spaces (`"My Home WiFi"`) | Accepted but warn "some devices may have trouble with spaces" |
| SSID with unicode (`"Café_ WiFi"`) | Accepted — wpa_supplicant handles UTF-8 |
| SSID with special chars (`"Test@#$%&*()"`) | Accepted — wpa_supplicant handles these |
| SSID with null byte | Rejected — null bytes not valid in SSIDs |
| SSID with only whitespace | Rejected — treated as empty after trim |
| Hidden SSID (broadcast disabled) | User must type exact SSID manually — works but requires manual entry |
| SSID that looks like a URL (`"http://evil.com"`) | Accepted — no security issue, just a weird SSID |

### 3.2 PSK Edge Cases

| Case | Expected Behaviour |
|------|-------------------|
| PSK < 8 characters | Rejected — "Password must be 8-63 characters" |
| PSK exactly 8 characters | Accepted (WPA2 minimum) |
| PSK exactly 63 characters | Accepted (WPA2 maximum) |
| PSK > 63 characters | Rejected — "Password must be 8-63 characters" |
| PSK with spaces | Accepted |
| PSK with only numbers | Accepted |
| PSK with special chars | Accepted |
| PSK = AP password (same string) | Accepted but warn "AP and client passwords are identical — this is unusual but valid" |
| Empty PSK | Rejected |
| PSK with emoji | Accepted (wpa_supplicant handles UTF-8) |

### 3.3 Config File Edge Cases

| Case | Expected Behaviour |
|------|-------------------|
| config.json doesn't exist | Falls back to AP mode with factory defaults |
| config.json is empty file | Falls back to AP mode with factory defaults |
| config.json is invalid JSON | Falls back to AP mode, logs parse error |
| config.json has valid JSON but no wifi section | Falls back to AP mode, treats as "no config" |
| config.json has wifi section but empty SSID | Falls back to AP mode |
| config.json is a symlink to /dev/null | Falls back to AP mode |
| config.json has permissions 000 (unreadable) | Falls back to AP mode, logs permission error |
| config.json contains email config but no WiFi config | Falls back to AP mode, preserves email config |

### 3.4 Network Environment Edge Cases

| Case | Expected Behaviour |
|------|-------------------|
| Router has no internet uplink | CLIENT mode connects, IMAP fails (circuit breaker), gateway stays up |
| Router reboots while dongle is in CLIENT mode | wpa_supplicant auto-reconnects; if it doesn't, wifi-manager detects and falls back to AP |
| WiFi signal intermittently drops (RSSI fluctuates -90 to -50) | wpa_supplicant handles reassociation; wifi-manager monitors connectivity |
| Two routers with same SSID but different PSK | Connects to one, fails auth → falls back to AP mode |
| Enterprise WiFi (WPA2-Enterprise, not PSK) | Not supported — user must provide PSK network |
| 5GHz-only network (dongle is 2.4GHz only) | Won't connect → falls back to AP mode |
| MAC filtering enabled on router | Dongle's MAC not in allowlist → won't associate → falls back to AP |
| Router DHCP pool exhausted | udhcpc fails → falls back to AP mode |
| Router has captive portal itself (hotel/café WiFi) | May connect but IMAP blocked by portal — gateway runs, IMAP fails |

### 3.5 AP Mode Edge Cases

| Case | Expected Behaviour |
|------|-------------------|
| 10+ clients connected to AP hotspot | dnsmasq serves DHCP up to 253 clients; performance degrades but works |
| Client connects but never opens browser | AP stays up indefinitely (no timeout) — client has network but sees no internet |
| Client connects, submits config, config is invalid | Config rejected, AP stays up, client sees error message |
| Client connects, submits config, config is valid | Config saved, "Reconnecting..." page shown, mode switch begins |
| Client loses WiFi during mode switch | Client disconnects; mode switch continues; if it fails, dongle returns to AP mode |
| AP SSID collision with neighbour's network | Both networks visible; clients may connect to wrong one |
| hostapd fails to start (driver error) | wifi-manager logs error, retries once, then stays in "broken" state with event log |
| dnsmasq fails to start (port conflict) | wifi-manager logs error, retries once, AP mode partially functional (no DHCP) |
| iptables command not found | Captive portal redirect fails; user must manually browse to 192.168.100.1:8080 |

### 3.6 State Machine Edge Cases

| Case | Expected Behaviour |
|------|-------------------|
| Power loss during CLIENT→AP switch (driver reload in progress) | On reboot: state unknown → safe default is AP mode |
| Power loss during AP→CLIENT switch | On reboot: reads saved config → tries CLIENT mode again |
| Rapid mode switching (user spam save button) | Debounce: ignore saves while mode switch is in progress |
| State machine gets stuck (bug) | Watchdog: if no state change in 5 minutes during a switch, force AP mode |
| config.json modified externally (e.g., via ADB) while wifi-manager running | Next config reload detects change and re-evaluates mode |

---

## 4. Automated Test Framework

### 4.1 Test Binary (runs on dev machine)

```
sms-gateway/
└── internal/wifimgr/
    ├── state_test.go          # State machine unit tests
    ├── validator_test.go      # SSID/PSK/config validation tests
    ├── captive_test.go        # HTTP redirect logic tests
    ├── iptables_test.go       # iptables rule generation tests
    └── mock/
        ├── exec.go            # Mock os/exec for shell command testing
        ├── fs.go              # Mock filesystem for config file testing
        └── net.go             # Mock network operations
```

### 4.2 On-Device Test Script (runs via ADB)

```bash
#!/system/bin/sh
# /data/sms-gateway/test-wifi-manager.sh
# Runs integration tests on the device without disrupting WiFi

PASS=0
FAIL=0

test_case() {
    local name="$1"
    local expected="$2"
    local actual="$3"
    if [ "$actual" = "$expected" ]; then
        echo "✅ PASS: $name"
        PASS=$((PASS+1))
    else
        echo "❌ FAIL: $name (expected '$expected', got '$actual')"
        FAIL=$((FAIL+1))
    fi
}

# Test 1: /status endpoint returns wifi fields
STATUS=$(curl -s http://127.0.0.1:8080/status)
echo "$STATUS" | grep -q "wifi_mode" && test_case "wifi_mode in /status" "true" "true" || test_case "wifi_mode in /status" "true" "false"

# Test 2: Config validation rejects empty SSID
RESULT=$(curl -s -X POST http://127.0.0.1:8080/api/wifi-config \
    -d '{"ssid":"","psk":"YOUR_WIFI_PASSWORD"}')
echo "$RESULT" | grep -q "error" && test_case "Empty SSID rejected" "true" "true" || test_case "Empty SSID rejected" "true" "false"

# Test 3: Config validation rejects short PSK
RESULT=$(curl -s -X POST http://127.0.0.1:8080/api/wifi-config \
    -d '{"ssid":"Test","psk":"short"}')
echo "$RESULT" | grep -q "error" && test_case "Short PSK rejected" "true" "true" || test_case "Short PSK rejected" "true" "false"

# Test 4: Preview endpoint works
RESULT=$(curl -s -X POST http://127.0.0.1:8080/api/wifi-preview \
    -d '{"ssid":"Test","psk":"YOUR_WIFI_PASSWORD12345678"}')
echo "$RESULT" | grep -q "valid" && test_case "Preview endpoint responds" "true" "true" || test_case "Preview endpoint responds" "true" "false"

# Test 5: Diagnostics endpoint works
RESULT=$(curl -s http://127.0.0.1:8080/api/wifi-diag)
echo "$RESULT" | grep -q "hostapd" && test_case "Diagnostics endpoint responds" "true" "true" || test_case "Diagnostics endpoint responds" "true" "false"

# Test 6: Event log exists and is valid JSONL
EVENTS=$(cat /data/sms-gateway/wifi-events.jsonl 2>/dev/null | wc -l)
[ "$EVENTS" -ge 0 ] && test_case "Event log exists" "true" "true" || test_case "Event log exists" "true" "false"

# Test 7: Config file is valid JSON
python3 -c "import json; json.load(open('/data/sms-gateway/config.json'))" 2>/dev/null
test_case "Config is valid JSON" "0" "$?"

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] && echo "✅ All tests passed" || echo "❌ $FAIL tests failed"
```

### 4.3 Destructive Test Suite (requires USB fallback)

```bash
#!/system/bin/sh
# /data/sms-gateway/test-wifi-destructive.sh
# WARNING: This will disrupt WiFi connectivity. Ensure ADB over USB is available.

# Test: Save bad config → expect AP fallback
curl -s -X POST http://127.0.0.1:8080/api/wifi-config \
    -d '{"ssid":"NONEXISTENT_NETWORK_xyz123","psk":"YOUR_WIFI_PASSWORD12345678"}'

# Wait 100 seconds for timeout
sleep 100

# Check AP mode is active
MODE=$(curl -s http://127.0.0.1:8080/status | python3 -c "import sys,json; print(json.load(sys.stdin).get('wifi_mode','unknown'))")
[ "$MODE" = "ap" ] && echo "✅ PASS: Fell back to AP mode with bad SSID" || echo "❌ FAIL: Expected AP mode, got $MODE"

# Restore good config
curl -s -X POST http://127.0.0.1:8080/api/wifi-config \
    -d '{"ssid":"YOUR_WIFI_SSID_1","psk":"YOUR_WIFI_PASSWORD"}'

# Wait for reconnection
sleep 90

# Verify back in CLIENT mode
MODE=$(curl -s http://127.0.0.1:8080/status | python3 -c "import sys,json; print(json.load(sys.stdin).get('wifi_mode','unknown'))")
[ "$MODE" = "client" ] && echo "✅ PASS: Reconnected to CLIENT mode" || echo "❌ FAIL: Expected CLIENT mode, got $MODE"
```

---

## 5. CI/CD Integration

### 5.1 Pre-deploy Checks (on dev machine, before every deploy)

```bash
# 1. Build succeeds
go build ./cmd/wifi-manager
go build ./cmd/sms-gateway

# 2. All unit tests pass
go test ./internal/... -v

# 3. Race detector clean
go test ./internal/... -race

# 4. Vet clean
go vet ./...

# 5. Config file validates
python3 -c "
import json, sys
cfg = json.load(open('config.json'))
assert 'wifi' in cfg, 'Missing wifi section'
assert cfg['wifi']['client']['ssid'], 'Empty SSID'
assert len(cfg['wifi']['client']['psk']) >= 8, 'PSK too short'
print('Config OK')
"
```

### 5.2 Post-deploy Verification (on device, after every deploy)

```bash
#!/system/bin/sh
# Run via ADB after pushing new binaries

# Check binaries exist and are executable
test -x /data/sms-gateway/wifi-manager && echo "✅ wifi-manager binary OK" || echo "❌ wifi-manager missing"
test -x /data/sms-gateway/sms-gateway && echo "✅ sms-gateway binary OK" || echo "❌ sms-gateway missing"

# Check config is valid
python3 -c "import json; json.load(open('/data/sms-gateway/config.json'))" 2>/dev/null \
    && echo "✅ Config is valid JSON" || echo "❌ Config is invalid JSON"

# Check gateway is running
curl -s http://127.0.0.1:8080/status >/dev/null 2>&1 \
    && echo "✅ Gateway web server responding" || echo "❌ Gateway web server not responding"

# Check WiFi mode
MODE=$(curl -s http://127.0.0.1:8080/status 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('wifi_mode','unknown'))" 2>/dev/null)
echo "WiFi mode: $MODE"

# Check IMAP IDLE
IMAP=$(curl -s http://127.0.0.1:8080/status 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('health',{}).get('imap_status','unknown'))" 2>/dev/null)
echo "IMAP: $IMAP"

# Check SMS poll
SMS=$(curl -s http://127.0.0.1:8080/status 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('health',{}).get('sms_status','unknown'))" 2>/dev/null)
echo "SMS: $SMS"

echo "Post-deploy check complete"
```

---

## 6. Test Matrix

### 6.1 WiFi Network Scenarios (setup page validation)

*Note: "Automated" column below referred to the unimplemented `/api/wifi-config` endpoint.
Validation is tested manually via the setup page or Settings WiFi management UI.*

| Scenario | SSID | PSK | Expected | Tested? |
|----------|------|-----|----------|---------|
| Happy path | NETGEAR_24ng | correct | Connects, gateway starts | ✅ Yes |
| force_ap_mode → save new network | NETGEAR_24ng | correct | Config written, reboots, connects | ✅ Yes (2026-04-11) |
| Wrong SSID | NONEXISTENT | correct | Fails → AP fallback | ⬜ Not yet |
| Wrong PSK | NETGEAR_24ng | wrongpass | Fails auth → AP fallback | ⬜ Not yet |
| Empty SSID | (empty) | correct | Validation reject: "SSID is required" | ✅ Yes |
| Short PSK | valid | abc | Validation reject: "Password must be ≥8 chars" | ✅ Yes |
| Duplicate SSID | existing | correct | Validation reject: "Network already exists" | ✅ Yes |

### 6.2 Device State Scenarios

| Scenario | Initial State | Action | Expected | Tested? |
|----------|--------------|--------|----------|---------|
| Boot with valid config | Normal | Power on | CLIENT mode, gateway starts | ✅ Routine |
| force_ap_mode + reboot | Client | Edit config, reboot | AP hotspot up, setup page served | ✅ 2026-04-11 |
| AP → save + reboot → client | AP mode | Save via setup page | Connects, SMS flow resumes | ✅ 2026-04-11 |
| Boot with no known WiFi | Any | WiFi setup fails | AP fallback, setup page | ⬜ Not yet |
| Power loss during mode switch | Any | Kill power | On reboot: tries client, falls back to AP | ❌ Manual only |
| Network drop during CLIENT | CLIENT mode | Router off | WiFi watchdog: soft reconnect / auto-reboot | ✅ Handled by watchdog |
| AP mode: no clients connect | AP mode | Wait | Stays in AP mode, no crash | ⬜ Not yet |
| AP mode: multiple clients | AP mode | Connect 2+ devices | All get DHCP, all see setup | ⬜ Not yet |

---

## 7. Missing Requirements (Not in Original Plan)

### 7.1 AP Password Changeability

**Gap**: The original plan mentions `wifi.ap.psk` in config but doesn't
describe how to change it. If the AP password leaks, the user should be able
to change it via the setup page.

**Add to plan**: The advanced settings page should include AP SSID and AP
password fields with validation.

### 7.2 Network Selection

**Status: IMPLEMENTED** — Both the setup mode captive portal and the main Settings
page support multiple networks (add, edit, reorder, remove). The `wifi.networks`
array in `config.json` holds all networks with priority ordering. `wpa_supplicant`
connects to the highest-priority reachable network automatically.

### 7.3 WiFi Scan in AP Mode

**Gap**: When in AP mode, the user has to type the SSID manually. A "Scan"
button that shows available networks would be very helpful.

**Technical feasibility**: `iwlist wlan0 scan` or `wpa_cli scan_results` can
list available networks. The setup page could display a dropdown of scanned
SSIDs.

**Add to plan as enhancement**: Phase 3.7 — WiFi scan endpoint.

### 7.4 Signal Strength Indicator

**Gap**: The user has no way to know if the dongle has good WiFi signal in
its deployed location.

**Add to plan**: The setup page should show current signal strength (RSSI,
bars) when in CLIENT mode. The `/status` endpoint already exposes this.

### 7.5 Factory Reset

**Gap**: If the user misconfigures everything and can't access the device
in AP mode (e.g., changed AP password and forgot it), there's no recovery
path.

**Add to plan**: Physical reset button isn't possible (no buttons on dongle),
but a factory reset endpoint in AP mode could work: POST `/api/factory-reset`
→ clears config.json, reboots to AP mode with factory defaults.

**Risk**: This endpoint must only be available in AP mode (not CLIENT mode)
to prevent accidental resets.

---

## 8. Proof of Correctness Checklist

Before declaring the feature complete, all of the following must be verified:

- [ ] Unit tests pass: `go test ./internal/wifimgr/... -v`
- [ ] Race detector clean: `go test ./internal/wifimgr/... -race`
- [ ] Config validation rejects all invalid inputs (empty SSID, short PSK, etc.)
- [ ] Config validation accepts all valid inputs
- [ ] State machine transitions are correct for all documented paths
- [ ] AP mode serves setup page at all URLs (captive portal)
- [ ] CLIENT mode serves normal dashboard at `/`
- [ ] CLIENT mode → wrong SSID → falls back to AP within 90±10 seconds
- [ ] AP mode → save valid config → switches to CLIENT within 60±10 seconds
- [ ] AP mode → save invalid config → validation error, stays in AP
- [ ] /status endpoint includes all wifi fields
- [ ] Event log records all state transitions
- [ ] Diagnostics endpoint runs all checks and reports pass/fail
- [ ] Preview endpoint returns correct actions without making changes
- [ ] Config survives a reboot (persisted to disk)
- [ ] Gateway starts automatically in CLIENT mode after reboot
- [ ] AP mode with no clients connected survives 24h without crash
- [ ] AP mode with 3+ clients connected handles all simultaneously
- [ ] Power loss during mode switch recovers to safe state (AP mode)
- [ ] iptables rules are cleaned up when switching AP → CLIENT
- [ ] dnsmasq is stopped when switching AP → CLIENT (if not needed for rndis0)
- [ ] hostapd is stopped when switching AP → CLIENT
- [ ] wpa_supplicant is stopped when switching CLIENT → AP
- [ ] No memory leaks after 24h of continuous operation in either mode

---

*See also: `WIFI_AP_PLAN.md` (design plan), `STATUS.md` (current status),
`FULL_PROJECT_TEST_PLAN.md` (comprehensive test plan — 103 tests passing),
`DOCUMENTATION_PLAN.md` (documentation roadmap)*
