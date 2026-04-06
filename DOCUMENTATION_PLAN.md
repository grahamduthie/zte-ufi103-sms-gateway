# Documentation Plan — SMS Gateway Project

*Created: 2026-04-05*
*Audience: Future users, administrators, and developers*

---

## Current State

The project has 8 markdown files but no entry point. All files are technically
correct but scattered. There is no single document that answers:

- **"What is this project?"** — for a new person encountering the codebase
- **"How do I set it up?"** — for a deployer
- **"How do I use it?"** — for the end user (Graham)
- **"How does it work?"** — for a developer extending the code
- **"What do I do when it breaks?"** — for an operator

---

## Proposed Documentation Structure

### Tier 1: Entry Points (for everyone)

| File | Audience | Purpose |
|------|----------|---------|
| `README.md` | Everyone | **New file** — Table of contents, project overview, quick start, links to all other docs |
| `CHANGELOG.md` | Everyone | **New file** — Chronological log of changes, deployments, incidents |

### Tier 2: User & Administrator Guides

| File | Audience | Purpose | Status |
|------|----------|---------|--------|
| `USER_GUIDE.md` | End user (Graham) | How to use the web UI, send SMS, read replies, check status | **New** |
| `ADMIN_GUIDE.md` | Administrator | Deployment, WiFi setup, monitoring, log reading, troubleshooting | **New** |
| `TROUBLESHOOTING.md` | Administrator | FAQ: "gateway not responding", "IMAP down", "WiFi won't connect" | **New** |
| `DEVICE.md` | Administrator | Hardware reference, root, partition map, AT commands | ✅ Exists |
| `STATUS.md` | Administrator | Current status dashboard | ✅ Exists (needs pruning) |

### Tier 3: Developer Reference

| File | Audience | Purpose | Status |
|------|----------|---------|--------|
| `GATEWAY.md` | Developer | Architecture, goroutines, data flow, config schema | ✅ Exists |
| `TESTING.md` | Developer | How to run tests, what they cover, adding new tests | **Rename of FULL_PROJECT_TEST_PLAN.md** |
| `WIFI_AP_PLAN.md` | Developer | WiFi manager design plan | ✅ Exists |
| `WIFI_AP_TEST_PLAN.md` | Developer | WiFi manager test plan | ✅ Exists |
| `BUGS.md` | Developer | Bug history, root causes, fixes | ✅ Exists |
| `REFACTOR_PLAN.md` | Developer | Completed refactoring items | ✅ Exists |

### Tier 4: Operational

| File | Audience | Purpose | Status |
|------|----------|---------|--------|
| `BACKUP.md` | Administrator | What was backed up, when, how to restore | **New** |
| `SECURITY.md` | Developer/Admin | Threat model, security posture, hardening steps | **New** |

---

## Implementation Plan

### Phase 1: Entry Points (do first)

**`README.md`** — The single entry point. Contents:
```
# ZTE UFI103 SMS Gateway

## What is this?
A self-contained SMS↔email bridge running on a ZTE UFI103 4G dongle.
Texts sent to the dongle's SIM are forwarded as emails. Email replies
are sent back as SMS messages.

## Quick Start
- Build and deploy: 3 commands
- Check status: curl command
- Web UI: URL

## Documentation Map
| I want to... | Read... |
|-------------|---------|
| Understand the system | GATEWAY.md |
| Set up the device | ADMIN_GUIDE.md |
| Use the web UI | USER_GUIDE.md |
| Fix a problem | TROUBLESHOOTING.md |
| Understand the hardware | DEVICE.md |
| Run tests | TESTING.md |
| See what changed | CHANGELOG.md |
| See known bugs | BUGS.md |
```

**`CHANGELOG.md`** — Chronological log. Format:
```
# Changelog

## 2026-04-05 — Test Plan Implementation
- Fixed shell injection in send_shell.go
- Added 103 automated tests across 5 packages
- Fixed decodeQuotedPrintable bug (multi-byte UTF-8)
- Added CheckIntegrity() and GetSendQueueStats()

## 2026-04-04 — IMAP IDLE + SIM Unlock Refactor
- Replaced 60s IMAP polling with persistent IDLE connection
- Proactive SIM unlock in SMS poller
- Removed SIM PIN lock permanently

## 2026-04-03 — Project Start
- Permanent root achieved via /system/xbin/librank
- SMS gateway built and deployed
```

### Phase 2: User & Admin Guides

**`USER_GUIDE.md`** — For Graham (non-technical end user):
```
# User Guide — SMS Gateway

## Checking Your Emails
- Texts sent to +447700000002 arrive in your Gmail as emails
- Subject: [SMS xxxxxxxx] From +44...
- Reply to the email to send a text back
- Keep replies under 160 characters

## Checking the Gateway Status
- On your phone connected to the same WiFi: http://172.16.10.226/ (port 80, password: mfm)
- Dashboard shows: signal strength, messages sent/received, recent texts

## Sending a Text from the Web UI
1. Open http://172.16.10.226/compose
2. Enter phone number (e.g. +447700000001)
3. Type your message (max 160 chars)
4. Click Send

## Common Questions
- How long does email→SMS take? (seconds, via IMAP IDLE)
- What if I send a reply longer than 160 chars? (truncated)
- Can I text multiple people? (yes, but each needs a separate thread)
```

**`ADMIN_GUIDE.md`** — For whoever maintains the device:
```
# Administrator Guide

## Overview
- What the system does
- Network topology diagram

## Initial Setup
- Root the device
- Build and deploy gateway
- Switch WiFi to client mode

## After a Reboot
- Run wifi-client-start.sh (or wifi-manager when implemented)
- Wait 60s, check /status

## Monitoring
- Web UI dashboard
- /status JSON endpoint
- What "healthy" looks like

## WiFi Configuration
- Switching AP ↔ client mode
- Updating WiFi credentials
- What to do if WiFi won't connect

## Updating the Gateway Binary
- Build, push .new, mv, restart
- Rolling back

## Log Files
- Location, rotation, what to look for

## Backup and Restore
- What to back up (config.json, sms.db)
- How to restore
```

**`TROUBLESHOOTING.md`** — Quick-reference FAQ:
```
# Troubleshooting

## Gateway not responding
1. Check ADB: sudo adb shell "ps | grep sms-gateway"
2. If not running: restart via start.sh
3. If running but no web: check WiFi connection

## SMS not arriving as emails
1. Check SIM count: /status → sms_status
2. Check gateway log: tail -f sms-gateway.log
3. Check SMSC: AT+CSCA? should return +447356000010

## Email replies not arriving as SMS
1. Check IMAP status: /status → imap_status
2. Check authorised senders list in config
3. Check send queue: /sent page

## WiFi won't connect after reboot
1. Run wifi-client-start.sh
2. Check wpa_supplicant.conf SSID is case-sensitive
3. Check router is broadcasting 2.4GHz (dongle is 2.4GHz only)

## "Cannot assign requested address" on wlan0
- wlan0 is still a member of bridge1
- Run: brctl delif bridge1 wlan0, then reload driver

## Database is locked
- Usually caused by concurrent writes
- SetMaxOpenConns(1) prevents this — verify config

## High CPU or memory usage
- respBuf leak was fixed — verify buffer size in /status
- Check goroutine count (should be 5 + 1 web server)
```

### Phase 3: Developer Reference

**`TESTING.md`** (rename from `FULL_PROJECT_TEST_PLAN.md`):
- Keep all current content (103 tests, corner cases, security findings)
- Add a "How to run tests" section at the top
- Add "How to add new tests" section

```markdown
## Running Tests

```bash
# All tests (on dev machine)
cd sms-gateway && go test ./...

# Specific package
go test ./internal/email/... -v

# Race detector
go test ./... -race

# Coverage
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

## Adding New Tests
1. Create `package_test.go` next to the code
2. Use table-driven tests for multiple inputs
3. Use `t.TempDir()` for database tests
4. Mock HTTP handlers with `httptest`
5. Run `go test ./...` before committing
```

**`SECURITY.md`**:
```
# Security Model

## Threat Model
- Physical access → full root (librank SUID binary)
- Network access → web UI on port 8080 (no auth)
- Config file → contains email credentials (chmod 600)

## Current Posture
- ✅ Shell injection fixed (send_shell.go)
- ✅ Input validation on all SMS send paths
- ✅ Config file permissions (chmod 600)
- ⚠️ Web UI has no authentication
- ⚠️ TLS skips certificate verification (no CA certs on device)
- ⚠️ Email passwords in plaintext config file

## Hardening Recommendations
1. Add basic auth to web UI (configurable)
2. Restrict web UI to local network only (already on 192.168.x.x)
3. Encrypt config file (optional, for high-security deployments)
4. Rotate email password periodically
```

**`BACKUP.md`**:
```
# Backup and Restore

## What's Backed Up
```
backup/
├── sbl1.img       # 525KB — Firehose programmer (IS the programmer for this device)
├── aboot.img      # 1MB — Android bootloader
├── hyp.img        # 525KB — Hypervisor
├── tz.img         # 525KB — TrustZone
├── rpm.img        # 525KB — Resource Power Manager
├── boot.img       # 17MB — Boot image (kernel + ramdisk)
├── recovery.img   # 17MB — Recovery image
└── modem.img      # 67MB — Modem firmware
```

## What to Back Up Regularly
- `/data/sms-gateway/config.json` — credentials and settings
- `/data/sms-gateway/sms.db` — message database
- `/data/misc/wifi/wpa_supplicant.conf` — WiFi credentials

## How to Back Up
```bash
sudo adb pull /data/sms-gateway/config.json ./config-backup.json
sudo adb pull /data/sms-gateway/sms.db ./sms-backup.db
sudo adb pull /data/misc/wifi/wpa_supplicant.conf ./wpa-backup.conf
```

## How to Restore
```bash
sudo adb push config-backup.json /data/sms-gateway/config.json
sudo adb push sms-backup.db /data/sms-gateway/sms.db
sudo adb shell "/system/xbin/librank /system/bin/sh -c 'chmod 600 /data/sms-gateway/config.json'"
# Then restart the gateway
```
```

### Phase 4: Pruning & Consolidation

| File | Action | Reason |
|------|--------|--------|
| `STATUS.md` | Keep as live dashboard reference | Current useful content |
| `DEVICE.md` | Keep as-is | Comprehensive hardware reference |
| `GATEWAY.md` | Keep as-is | Good architecture doc |
| `BUGS.md` | Keep as-is | Good bug history |
| `REFACTOR_PLAN.md` | Archive — move to `archive/` | All items completed, historical only |
| `FULL_PROJECT_TEST_PLAN.md` | Rename to `TESTING.md` | Better name |
| `WIFI_AP_PLAN.md` | Keep as-is | Future feature design |
| `WIFI_AP_TEST_PLAN.md` | Keep as-is | Future feature tests |
| `README.md` | **Create** | Entry point |
| `CHANGELOG.md` | **Create** | Change log |
| `USER_GUIDE.md` | **Create** | End user guide |
| `ADMIN_GUIDE.md` | **Create** | Admin guide |
| `TROUBLESHOOTING.md` | **Create** | FAQ |
| `SECURITY.md` | **Create** | Security model |
| `BACKUP.md` | **Create** | Backup/restore guide |

---

## File Tree After Documentation

```
/home/marlowfm/dongle/
├── README.md                    ← NEW: Entry point
├── CHANGELOG.md                 ← NEW: Change log
├── USER_GUIDE.md                ← NEW: End user guide
├── ADMIN_GUIDE.md               ← NEW: Admin guide
├── TROUBLESHOOTING.md           ← NEW: FAQ / troubleshooting
├── SECURITY.md                  ← NEW: Security model
├── BACKUP.md                    ← NEW: Backup and restore
├── STATUS.md                    ← Current status dashboard
├── DEVICE.md                    ← Hardware reference
├── GATEWAY.md                   ← Software architecture
├── BUGS.md                      ← Bug history
├── TESTING.md                   ← Renamed from FULL_PROJECT_TEST_PLAN.md
├── WIFI_AP_PLAN.md              ← WiFi manager design
├── WIFI_AP_TEST_PLAN.md         ← WiFi manager test plan
├── REFACTOR_PLAN.md             ← Historical (all items completed)
└── sms-gateway/
    └── ... (source code with tests)
```

---

*This plan should be reviewed before implementation begins. Each new file is
estimated at 30-60 minutes of writing time.*
