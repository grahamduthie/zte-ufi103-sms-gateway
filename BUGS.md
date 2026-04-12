# Known Bugs — History & Fixes

## Fixed Bugs (original development)

### Bug 1: SIM PIN Lock Blocks SMS Delivery
**Symptom**: SMS sent to the dongle don't arrive on the SIM for 40+ minutes.
**Root cause**: After WiFi mode switch, the SIM re-locks. The SMSC queues
messages but doesn't deliver them until `AT+CPIN="8837"` is sent.
**Fix**: `SendSMS()` calls `ensureUnlocked()` before AT commands.
Additionally, the SIM PIN lock has been **permanently disabled** via
`AT+CLCK="SC",0,"8837"` — the SIM no longer requires a PIN on boot.
`ensureUnlocked()` returns `nil` on timeout or ERROR (RILD interference)
rather than failing the poll — `GetSMSCount` is the real health indicator.
**Status**: ✅ Fixed + SIM PIN permanently removed.

### Bug 2: SIM Index Dedup Rejects New Messages
**Symptom**: New SMS arrive on the SIM but are never imported to the database.
**Root cause**: When messages are deleted from the SIM, their `sim_index` stayed
in the database. New messages reuse freed slots (0, 1, 2...) and were rejected
as duplicates by `MessageExistsBySIMIndex`.
**Fix**: `MarkDeletedFromSIM` now sets `sim_index = NULL`. All queries use
`COALESCE(sim_index, -1)` to handle NULL values.
**Status**: ✅ Fixed.

### Bug 3: Email Reply Contains MIME Headers
**Symptom**: SMS received from email replies contain raw MIME headers.
**Root cause**: The IMAP poller was reading the raw RFC822 body instead of
extracting just the `text/plain` part.
**Fix**: `extractPlainFromBody()` finds the `Content-Type: text/plain` section
and extracts only the body text between MIME boundaries.
**Status**: ✅ Fixed.

### Bug 4: Email Reply Includes Quoted Original Text
**Symptom**: SMS from email replies include "On Sat, 4 Apr 2026 at 13:41,
Graham Duthie wrote:" and the original message.
**Root cause**: Gmail appends the original email as quoted text.
**Fix**: `cleanReplyBody()` strips "On ... wrote:" blocks, Outlook headers,
`> ` quoted lines, and everything after `\n---`.
**Status**: ✅ Fixed.

### Bug 5: Quoted-Printable Encoding Corrupts Apostrophes
**Symptom**: Email replies with apostrophes show as `Here=E2=80=99s` in SMS.
**Root cause**: Gmail encodes smart quotes (U+2019) as quoted-printable `=E2=80=99`.
**Fix**: Added `decodeQuotedPrintable()` and `normaliseToGSM()`.
**Status**: ✅ Fixed.

### Bug 6: NULL sim_index Crashes Database Queries
**Symptom**: `sql: Scan error … converting NULL to int is unsupported`.
**Root cause**: After `MarkDeletedFromSIM` sets `sim_index = NULL`, queries fail.
**Fix**: All queries use `COALESCE(sim_index, -1)`.
**Status**: ✅ Fixed.

### Bug 7: SIGHUP Kills Daemon on ADB Disconnect
**Symptom**: Gateway dies silently when the adb shell session ends.
**Fix**: Added `signal.Ignore(syscall.SIGHUP)` at daemon startup.
**Status**: ✅ Fixed.

### Bug 8: Goroutine Panics Kill Polling Silently
**Symptom**: Gateway process is alive but stops polling.
**Fix**: Added `defer recover()` wrappers with logging around all goroutine loop bodies.
**Status**: ✅ Fixed.

---

## Fixed Issues (Refactor 2026-04-04)

### `respBuf` Unbounded Growth → FIXED
Truncated to `[:0]` at the start of every command. Peak buffer size bounded to
one command's response (~4KB) instead of growing for days.

### `readerLoop` Cannot Update After Reopen → FIXED
Removed `bufio.Reader`; reads one byte at a time directly from `s.file` with
500ms read deadline. A `fdMu sync.RWMutex` protects the `s.file` pointer.

### `sendSMSDirectAT` fd Sharing → BEST MITIGATION
A dedicated `SendSession` (separate fd) is not feasible: the SMD channel only
delivers AT responses to the first fd opened. Current design (shared fd) is
correct; the concurrent-write race from `reopen()` is eliminated because
`reopen()` has been removed.

### `parseCMGL` Skips Messages with Empty Bodies → FIXED
Rewrote to detect `+CMGL:` header lines and collect subsequent non-header lines
as the body. The outer loop advances to the last consumed body line.

### `decodeIfNeeded` False-Positive on Hex Strings → FIXED
After decoding, any byte below 0x20 (excluding `\n`, `\r`, `\t`) causes the
function to return the original text unchanged.

### Circuit Breaker for AT Failures → FIXED
Backoff sequence: 2s, 4s, 8s, 16s, 32s, 60s (capped). State in `daemon_health`.

### Send Queue Exponential Backoff → FIXED
`next_retry_at` column added. Retries at `10s * 2^attempts` capped at 300s/5min.
Max attempts raised to 50.

---

## Post-Refactor Fixes (2026-04-04 → 2026-04-05)

### Bug 9: Modem ERROR Terminal Included in SMS Body
**Symptom**: Forwarded emails had "ERROR" on a line by itself beneath the message.
**Root cause**: `parseCMGL` didn't break body collection on bare `ERROR`.
**Fix**: Added `ERROR`, `+CME ERROR:`, `+CMS ERROR:` to break conditions.
Added regression test `TestParseCMGL_ErrorTerminal`.
**Status**: ✅ Fixed 2026-04-04.

### Bug 10: Quoted-Printable Decode Fails on Multi-Byte UTF-8
**Symptom**: Smart quotes appear as `=E2=80=99` in SMS.
**Root cause**: `strconv.ParseInt(hex, 16, 8)` rejects bytes >127 (signed int8 overflow).
**Fix**: Changed to `strconv.ParseUint(hex, 16, 8)` with byte accumulation for UTF-8.
**Status**: ✅ Fixed 2026-04-05.

### Bug 11: GetSMSCount Reads RILD's CPMS Response Instead of Ours
**Symptom**: Inbound SMS arrive on the SIM but `GetSMSCount` reports `count=0`,
so the poller never calls `ListSMS` and messages are never imported.
**Root cause**: RILD sends `AT+CPMS?` every 3-5 seconds on the same shared
`/dev/smd11` fd. The gateway uses `sendCommandsMulti` which accumulates ALL
responses in `respBuf`. RILD's `AT+CPMS?` (which often shows `count=0` because
RILD reads SMS via QMI WMS immediately) arrives in the buffer **after** our
own `+CPMS: "SM",N,...` response. The old regex took the **last** `+CPMS`
match — RILD's `0` instead of our actual count.
**Fix**: After receiving the buffer, find the `AT+CPMS?\r\n` command we sent,
slice the buffer from that point forward, then parse. This isolates our response
from any interleaved RILD noise.
```go
pattern := "AT+CPMS?\r\n"
idx := strings.LastIndex(out, pattern)
if idx >= 0 { out = out[idx+len(pattern):] }
// Now parse only our response
```
**Status**: ✅ Deployed 2026-04-05 20:40 BST. Awaiting reboot confirmation that
the fix resolves the two consecutive missing texts.

---

## Boot Persistence Issue (Fixed 2026-04-05)

**Symptom**: Gateway daemon does not auto-start after reboot.
**Root cause**: Android init runs each service in its own cgroup. When the main
process exits, init tears down the entire cgroup — killing background children
even with `setsid`, `nohup`, or double-fork.
**Fix**: Added `sms-gw` as a named service in `/init.target.rc`. `start.sh` runs
the gateway in the **foreground** — it IS the service's main process, so init
never tears down the cgroup. `start.sh` also runs `wifi-setup.sh` on boot.
**Status**: ✅ Fixed 2026-04-05. Verified with full reboot test.

---

## SMS Reliability Issues (Investigated and Fixed 2026-04-05)

### Duplicate Gateway Processes Cause Missed Messages
**Symptom**: SMS messages sometimes not forwarded as emails.
**Root cause**: Two copies of `start.sh` / `sms-gateway` running simultaneously.
Both opened `/dev/smd11` (the SMD channel allows multiple concurrent opens) and
competed for AT responses — each process stole the other's modem replies,
causing `AT+CPMS?` and `AT+CMGL` responses to be misrouted.

**Why two instances?** There are two independent boot mechanisms:
1. The `sms-gw` init service in `/init.target.rc`
2. `/data/local/userinit.sh` which runs `librank sh start.sh &`

During development, manual invocations via `adb shell` added a third copy.

**Fix**: PID file guard in `start.sh`. On startup, writes `$$` to
`gateway.pid`. Any subsequent invocation checks if that PID is alive with
`kill -0`; if so, exits immediately.
**Status**: ✅ Fixed 2026-04-05.

### `busybox flock -n <fd>` Not Supported on This Device
**Symptom**: Every invocation of `start.sh` exited with "another instance
already running", causing init to throttle the service off.
**Root cause**: BusyBox v1.23.0 (2014) on this device does not support the
`flock -n <fd>` form (numeric file descriptor argument). `exec 9>file` silently
fails in busybox ash to open a persistent fd, so `flock -n 9` returns "Bad file
descriptor" (non-zero) on every call, triggering the `|| exit 1` branch.
**Fix**: Replaced `exec 9>file; flock -n 9` with a PID file approach.
**Status**: ✅ Fixed 2026-04-05.

### Log File Grows Unboundedly
**Symptom**: Every 2-second poll logged two lines ("SMS poll: starting" +
"SMS poll: count=0"), producing ~60 MB/day and filling the 1.6 GB `/data`
partition in ~3–4 weeks.
**Fix (two-part)**:
1. Removed per-poll "starting" and "count=0" log lines — only log when
   count > 0 or an error occurs.
2. Added hourly housekeeping goroutine (`housekeeping.go`) that rotates the
   log at 10 MB (keeping one `.1` backup = 20 MB max), runs `PRAGMA
   wal_checkpoint(TRUNCATE)`, and prunes records older than 90 days.
   `start.sh` also rotates at 5 MB on startup.
**Status**: ✅ Fixed 2026-04-05.

---

## Bug 12: AT+CNMI=0,0,0,0,0 Prevents SMS Storage in SM (Fixed 2026-04-06)

*For a full explanation of the underlying modem architecture, see `SMS_MODEM_ARCHITECTURE.md`.*

**Symptom**: `GetSMSCount` consistently returns 0. `AT+CPMS?` and `AT+CMGL="ALL"`
confirm the SIM is genuinely empty. Inbound texts never appear as emails.
Outbound (email→SMS) works fine. IMAP IDLE works fine.

**Root cause**: RILD sets `AT+CNMI=0,0,0,0,0` at boot. With `mt=0`, the modem
routes all incoming SMS exclusively via its internal QMI WMS channel to RILD
and does **not** write them to AT-accessible SM storage. Our gateway polls SM
via `AT+CMGL`, which is always empty.

**Fix**: Include `AT+CNMI=2,1,0,0,0` in the `GetSMSCount` command sequence
(sent every poll cycle). With `mt=1`, the modem stores each incoming SMS in the
AT+CPMS preferred storage (SM) AND sends a `+CMTI` unsolicited result code.
Our `readerLoop`→`NewMessageCh` path catches `+CMTI` for an immediate poll;
the 2-second polling loop catches anything the CMTI notification misses.

**Confirmed**: After fix, first test text received and forwarded within seconds.
`+CMTI` fires immediately on arrival, triggering sub-second detection.

**Status**: ✅ Fixed 2026-04-06.

---

## RILD / SMS Architecture Findings (updated 2026-04-06)

*See `SMS_MODEM_ARCHITECTURE.md` for the full research notes including the
theoretical risk of RILD resetting `AT+CNMI` and the potential alternative fix.*

These are factual findings, not bugs — recorded here to prevent future
misdiagnosis.

### SMS Storage and Deletion
On this MSM8916 device, RILD uses QMI WMS (over `/dev/smd36`) as its primary
SMS channel — NOT AT commands. With the correct `AT+CNMI=2,1,0,0,0` setting,
incoming SMS are stored on the SIM card (SM storage) by the modem. RILD does
NOT delete them from SM storage — they persist until our gateway reads them
with `AT+CMGL` and deletes them with `AT+CMGD`.

### +CMTI URCs Now Delivered (correction from 2026-04-05)
With `AT+CNMI=2,1,0,0,0` (set by our gateway on every poll), the modem DOES
send `+CMTI:` unsolicited result codes when a new SMS arrives. Our `readerLoop`
detects these and signals `NewMessageCh`, triggering an immediate poll. The
earlier finding that "+CMTI is consumed by RILD" was caused by RILD's `mt=0`
setting suppressing all notifications — once we override to `mt=1`, +CMTI
arrives on our fd correctly.

### mmssms.db Always Empty
`com.qualcomm.telephony` replaces the standard Android telephony provider on
this device. The Android telephony database
(`/data/data/com.android.providers.telephony/databases/mmssms.db`) is
permanently empty. The `pollAndroidSMS()` function in `android_sms.go` opens
the database successfully (gateway runs as root), queries it, finds 0 rows, and
returns silently. It is harmless dead code. Do not remove it without verifying
whether a firmware update has changed this behaviour.

---

## Bug 13: Email Reply SMS Text Includes AT Commands (Fixed — 2026-04-06)

### Symptom
When replying to an email (which the gateway forwards as an SMS), the text
message received on the phone contains RILD's AT commands before the actual
reply text. Example:

```
AT+CPMS="SM","SM","SM"

Reply to Malcolm text.
```

The AT commands appear first, followed by a blank line, then the user's actual
reply. The reply text itself is intact — it's prefixed by the AT noise.

### Root Cause
The `sendSMSDirectAT()` function in `internal/atcmd/session.go` sends AT
commands to `/dev/smd11` — a shared fd that RILD also writes to. RILD issues
`AT+CPMS?` every 3-5 seconds. When the gateway sends `AT+CMGS="+number"\r`,
the modem enters SMS text input mode. While in this mode, the modem captures
**all** data arriving on the fd as SMS body text — including RILD's
interleaved AT commands.

The timing is the problem: after `AT+CMGS`, there's a window between the modem
entering text input mode and the gateway sending the actual message text
(followed by Ctrl-Z). Any RILD AT commands arriving in this window get
captured as part of the SMS body.

### Attempted Fixes (All Failed)

#### Attempt 1: Wait for `> ` prompt with regex
**Approach**: Instead of a fixed 2-second sleep after `AT+CMGS`, poll respBuf
for the `> ` prompt. Once detected, send text immediately.

**Regex used**: `\[\SMS\s+([A-Za-z0-9-]{8,15})\]|\[([A-Za-z0-9-]{8,15})\]`
Wait — no, the regex was `(?:^|\r\n)>\s+\r\n`.

**Why it failed**: The regex matched false positives. When the modem is in
text input mode, it echoes RILD's AT commands back with `> ` prefix (e.g.
`> AT+CPMS="SM","SM","SM"`). The regex matched these echoed commands instead
of the actual modem prompt, causing the gateway to send text prematurely.

#### Attempt 2: Refined regex to match only standalone `>` prompt
**Approach**: Changed regex to `(?:\r\n|^)> $` — matches `> ` only at the end
of the buffer on its own line.

**Why it failed**: The modem prompt doesn't always arrive as `\r\n> ` at the
end of the buffer. RILD noise can arrive between the `>` and the rest of the
buffer, so the regex never matches within the 5-second timeout.

#### Attempt 3: Reduced fixed sleep to 500ms
**Approach**: Reverted to fixed sleep approach but reduced from 2s to 500ms
to minimise the window for RILD interference.

**Why it failed**: 500ms was not enough time for the modem to respond with
the `> ` prompt. The modem still hasn't responded when we send the text,
so the modem treats everything we send (including RILD's AT commands arriving
in the same window) as SMS body text.

### Key Observations
1. **This bug is NOT new** — it existed before this session. The old code used
   a 2-second sleep which is actually *more* likely to capture RILD noise
   than a shorter sleep. The fact that users reported this bug now suggests
   the old code also had the same issue, it just wasn't noticed.
2. **The modem captures everything on the fd as text** while in CMGS text
   input mode — there's no way to "block" RILD's writes.
3. **The 500ms sleep** approach *should* work if the modem responds fast
   enough, but it doesn't always.

### Files Changed (Session 2026-04-06 Email Format)

The following changes were made in this session and **are correct**:

| File | Change |
|------|--------|
| `internal/database/db.go` | `NextDailySequence` — date format changed from `YYYYMMDD` to `DDMMYY`; prefix length from 8 to 6 |
| `internal/database/db.go` | `MarkForwarded` — prefix slice from `[:8]` to `[:6]` |
| `internal/database/db.go` | `CreateEmailSession` — prefix slice from `[:8]` to `[:6]` |
| `internal/email/bridge.go` | `ForwardMessage` — subject format: `Text from +44... [060426-001]` (reference at end) |
| `internal/email/bridge.go` | `buildHTMLEmail` — From/Received/Reference table moved above message body; footer reply instructions removed |
| `internal/email/bridge.go` | `buildDeliveryHTML` — converted to HTML format matching new style |
| `internal/email/bridge.go` | `SetLogoBase64` — new function to load logo for email embedding |
| `internal/email/bridge.go` | `formatMultipartMessage` — MIME multipart email with logo as CID attachment |
| `internal/email/bridge.go` | `processReply` — regex changed to match both `[SMS YYYYMMDD-NNN]` (old) and `[DDMMYY-NNN]` (new) subjects |
| `cmd/sms-gateway/main.go` | Added `loadLogoBase64()` to load logo at startup |
| `cmd/sms-gateway/main.go` | `email.SetLogoBase64()` called after bridge creation |
| `internal/atcmd/session.go` | `sendSMSDirectAT` — SMS send timing reworked (still has Bug 13) |

### Final Fix (2026-04-06)

**Root cause (discovered)**: `readerLoop` only flushes bytes to `respBuf` when it
receives a `\n`. The modem's `>` prompt is `\r\n> ` — no trailing newline. Without
a newline, `>` never reached `respBuf`. `sendSMSDirectAT` was only ever detecting
`>` when RILD happened to follow it with a newline-terminated line (i.e. `AT+CPMS=...`),
which means we were always already corrupted by the time we detected the prompt.

**Full fix**: Two changes to `internal/atcmd/session.go`:

1. **`readerLoop`**: Added immediate flush when `>` is seen, without waiting for `\n`.
   Also signals `promptCh` (new buffered channel) to wake `sendSMSDirectAT` instantly.

2. **`sendSMSDirectAT`**: Replaced 20ms polling loop with `select` on `promptCh`.
   PDU is now written within microseconds of modem sending `>`, beating RILD's
   AT+CPMS injection (~2ms later).

Supporting changes made during this session:
- Switched from text mode (AT+CMGF=1) to PDU mode (AT+CMGF=0) — RILD injection
  now gives clean `+CMS ERROR: 304` instead of silently corrupting the SMS body.
- Moved AT+CNMI=2,1,0,0,0 from every-2s poll to startup-only (+ hourly housekeeping)
  — stopped RILD from being triggered on every poll cycle.
- Added "wait for quiet" after AT+CMGF=0 — flushes RILD's reaction to mode change.
- Added bare `OK` detection in Step 3 confirmation — some firmware returns `OK`
  without `+CMGS: <ref>`.

**Result**: `SMS sent to +447700000001 (ref=47)` — confirmed clean send, no AT
commands in received text.

| File | Change |
|------|--------|
| `internal/atcmd/session.go` | `readerLoop` — immediate flush + `promptCh` signal on `>` |
| `internal/atcmd/session.go` | `sendSMSDirectAT` — PDU mode, quiet-wait, `promptCh`-based prompt detection |
| `internal/atcmd/session.go` | `GetSMSCount` — removed AT+CNMI from every-poll cycle |
| `cmd/sms-gateway/main.go` | AT+CNMI applied once at startup via `SetTextMode` |
| `cmd/sms-gateway/housekeeping.go` | AT+CNMI re-applied hourly as safety net |

---

*See also: `STATUS.md` (current status), `GATEWAY.md` (architecture), `REFACTOR_PLAN.md` (fix plan), `DEVICE.md` (hardware and RILD details)*

---

## Bug 14: Stale PID File Blocks Boot After Abrupt Disconnect (Fixed — 2026-04-09)

### Symptom
After abruptly unplugging the USB cable (no clean shutdown), the gateway fails
to start on next boot. `start.sh` sees the old `gateway.pid` file, checks
whether the recorded PID is alive, and sometimes finds a different process
with that PID — causing `start.sh` to exit with "already running".

### Root cause
`gateway.pid` is written by `start.sh` and cleaned up via a `trap` on EXIT.
When the device is yanked without shutdown, the `trap` never fires and the
PID file persists. On next boot, the PID number may coincidentally be reused
by an unrelated process (e.g. `zygote`), causing `kill -0` to return success.

### Fix
Two mitigations:
1. **Shut Down Dongle** button added to the web UI Danger Zone — issues
   `setprop sys.powerctl shutdown` for a clean power-off, ensuring the trap
   fires and `gateway.pid` is removed.
2. **Boot persistence** now uses the `qrngp` wrapper (in `/init.target.rc`)
   instead of a named init service.

### Workaround
If the gateway won't start after boot, delete the stale PID file:
```bash
adb shell "busybox rm -f /data/sms-gateway/gateway.pid"
adb shell "setprop ctl.start sms-gw"
```

---

*See also: `STATUS.md` (current status), `GATEWAY.md` (architecture), `DEVICE.md` (hardware and RILD details)*

---

## Bug 15: WiFi Driver Instability — pronto_wlan.ko Crashes (Mitigated — 2026-04-10)

### Symptom
The web GUI becomes unreachable after a period of operation. The dongle is
running (modem OK, SMS polling works, IMAP connected) but WiFi has dropped.
`wlan0` has disappeared from the system entirely.

### Root cause (driver level)
The Qualcomm WCNSS PRONTO WiFi driver (`pronto_wlan.ko`) on this MSM8916
chipset has a hardware/firmware limitation: after 2-3 `wpa_supplicant` restart
cycles (kill + start) within a single boot session, the driver enters an
unrecoverable state. The `wlan0` network device disappears and cannot be
brought back. Only a clean reboot restores the driver.

### Trigger
- WiFi watchdog detecting lost IP and restarting wpa_supplicant
- Each soft reconnect (kill wpa_supplicant, restart) counts against the
  driver's limited reload budget
- After ~3 restarts, the driver crashes and wlan0 vanishes

### Mitigations implemented
1. **WiFi watchdog hardened** (`wifi_watchdog.go`):
   - Check interval: 120s (not 45s)
   - 3-minute boot grace period (no checks at all)
   - Exponential backoff on failure: 60s → 120s → 240s → 480s → 30min cap
   - Hard limit: 5 consecutive failures → stop trying until reboot
   - Missing wlan0 detection: if device is gone, stop immediately

2. **No driver reload in watchdog**: Never does `rmmod`/`insmod` at runtime.
   Only soft reconnects (wpa_supplicant restart).

3. **Clean shutdown button**: "Shut Down Dongle" in web UI prevents stale
   PID file issues that complicate reboots.

### Workaround
```bash
# When WiFi drops and web GUI is unreachable:
adb reboot
# Wait ~2 minutes for boot + WiFi + gateway to come back.
```

### Confirmed
- Driver crashes after ~2-3 wpa_supplicant restarts per boot session
- Reboot always recovers the driver to a fresh, stable state
- Gateway continues running fine even when WiFi is down (SMS polling works
  over the modem, IMAP/SMTP reconnect when WiFi returns)

---

## Bug 16: Old Watchdog Unlimited Retries Destroy Driver (Fixed — 2026-04-10)

### Symptom
After a brief WiFi disruption overnight, WiFi never recovered. `wlan0`
disappeared entirely and the web GUI was unreachable for ~7 hours until a
manual reboot.

### Root cause
The binary deployed before 2026-04-10 had `wifiCheckInterval = 45s` and **no
hard limit** on reconnect attempts. When WiFi dropped at ~04:02 BST, the
watchdog hammered wpa_supplicant kills-and-restarts every ~20–45 seconds with
no upper bound. Log evidence: over 50 reconnect attempts between 04:02 and
04:30 BST. This far exceeded the pronto driver's ~3-restart budget, causing
wlan0 to vanish permanently until reboot.

The root WiFi disruption was likely a brief router blip (common overnight
maintenance). With no retry limit, the watchdog turned a 30-second router
restart into a multi-hour outage.

### Fix
The hardened watchdog (5-failure hard limit, exponential backoff) was already
written (commits 2dcce5d + 5ab478b) but had not yet been deployed. The new
binary was deployed at 09:52 BST the same morning. The unlimited-retry binary
is no longer in use.

### Status
✅ Fixed — new binary deployed 2026-04-10 09:52 BST.

---

## Bug 17: WiFi Watchdog Grace Period Bug — Watchdog Checks Only Once (Fixed — 2026-04-10)

### Symptom
After the hardened watchdog was deployed, WiFi would still drop and not be
detected. The watchdog logged one check after boot then went silent.

### Root cause
The grace period was implemented as:
```go
select {
case <-grace.C:
default:
    continue
}
```
A `time.Timer` channel sends exactly one value when it fires. After the first
check past the grace period consumed that value, all subsequent iterations hit
`default: continue` — skipping the check permanently. The watchdog only ever
performed **one connectivity check** per gateway session.

### Fix
Replaced the channel drain with a `graceExpired bool`:
```go
if !graceExpired {
    select {
    case <-grace.C:
        graceExpired = true
    default:
        continue
    }
}
```
The watchdog now correctly checks every 2 minutes throughout the session.

### Files changed
| File | Change |
|------|--------|
| `cmd/sms-gateway/wifi_watchdog.go` | Added `graceExpired bool`; replaced channel drain with bool check |

### Status
✅ Fixed — 2026-04-10.

---

## Bug 18: start.sh Triggers Unnecessary Driver Reloads on Service Restart (Fixed — 2026-04-10)

### Symptom
WiFi would drop repeatedly within the same boot session even without the
watchdog doing anything. Each `adb reboot` gave stability for 30–60 minutes
before WiFi dropped again.

### Root cause
ModemManager's USB probing causes `sys.boot_completed` to cycle on the device,
which re-fires the Android init `on property:` trigger for the `sms-gw`
service. Init kills the entire service **cgroup** — including `wpa_supplicant`,
which runs as a background child started by `wifi-setup.sh`. The service is
then restarted.

The old `start.sh` WiFi check was binary: wlan0 has IP → skip; otherwise → run
`wifi-setup.sh`. Because init killed `wpa_supplicant` via cgroup teardown,
wlan0 had no IP when the new `start.sh` checked. This triggered a full
`rmmod`/`insmod` driver reload. Each reload adds wear; the pattern was:

1. Init re-triggers service → cgroup kills wpa_supplicant
2. New start.sh: wlan0 no IP → `wifi-setup.sh` (rmmod/insmod)
3. Repeat every 3–5 minutes while ModemManager is active
4. After 3–4 reloads within a session, driver becomes unstable

Log evidence from 2026-04-10: wifi-setup.sh was called 4 times within a
~90-minute window after a clean reboot, with no corresponding `wlan0` device
disappearance between calls.

### Fix
`start.sh` now uses a three-way check before deciding what to do:

1. **wlan0 has IP** → skip everything (most common case after gateway restart
   via web UI or crash-restart loop).
2. **wlan0 device exists but no IP** → soft reconnect: restart `wpa_supplicant`
   + run `udhcpc`. No `rmmod`/`insmod`. If the soft reconnect succeeds, done.
3. **wlan0 device missing** (or soft reconnect failed) → full `wifi-setup.sh`
   (rmmod/insmod). This handles first boot from AP mode and genuine driver
   crashes.

On first boot from AP mode, `wpa_supplicant.conf` does not yet exist, so
`wpa_supplicant` fails → soft reconnect fails → falls through to full
`wifi-setup.sh`. Correct behaviour is preserved.

### Files changed
| File | Change |
|------|--------|
| `scripts/start.sh` | WiFi section replaced with three-way check + soft reconnect |

### Status
✅ Fixed — 2026-04-10.

---

*See also: `STATUS.md` (current status), `GATEWAY.md` (architecture), `DEVICE.md` (hardware and RILD details)*


---

## Bug 19: WiFi Driver Crash Leaves GUI Unreachable Indefinitely (Fixed — 2026-04-11)

### Symptom
After the pronto_wlan driver crashes and `wlan0` disappears, the web GUI
remained unreachable until a manual reboot — potentially many hours.

### Root cause
The watchdog correctly detected `wlan0` missing and stopped all reconnect
attempts (to avoid making the driver worse), but then did nothing further.
There was no automatic recovery path. A manual `adb reboot` was required.

### Investigation
Research confirmed the Qualcomm WCNSS PRONTO driver bug is a known architectural
weakness in the Android 4.4-era driver. Key findings:

- **`rmmod`/`insmod` does not work**: The kernel module itself unloads fine
  (refcount is 0) but `wlan0` does not reappear. The RF subsystem firmware
  ("iris") is in a corrupt state that only a full hardware power-cycle resets.
  The vendor kernel (3.10.28) exposes no sysfs reset path — no
  `wcnss_wlan_state` or `pronto_reset` control node exists.
- **No patch available**: This is a closed vendor kernel. No upstream fixes,
  patched driver, or firmware update exists for this device.
- **Only a full reboot recovers it**, consistently and reliably.

On the day this was diagnosed (2026-04-11), the driver crashed at 19:15 BST
the previous evening (7 hours into the boot session). The watchdog ran its 5
soft-reconnect attempts with exponential backoff (19:15–19:38), then gave up.
The GUI was unreachable for ~13 hours until a manual reboot the next morning.

### Fix
`wifi_watchdog.go` — when `!wlan0Exists()` is detected after the boot grace
period, the watchdog now:

1. Logs the detection and waits 30 seconds (avoids false positives during mode
   switches, which can transiently remove/recreate wlan0)
2. Re-checks `wlan0Exists()`
3. If still missing: logs "pronto_wlan driver crashed" and triggers
   `/system/xbin/librank /system/bin/reboot`

Additionally, after 5 failed soft-reconnect attempts, the watchdog now
**continues running** (previously it stopped entirely). Soft reconnects are
suspended to protect the driver, but the existence check keeps firing every
2 minutes so a subsequent driver crash is still caught.

### Recovery timeline
- Crash occurs → wlan0 disappears
- Watchdog detects missing wlan0 on next 2-minute tick (up to 2 min delay)
- 30-second confirmation wait
- Reboot triggered; device back up in ~2 minutes
- **Worst-case GUI downtime: ~4.5 minutes**

### Files changed
| File | Change |
|------|--------|
| `cmd/sms-gateway/wifi_watchdog.go` | `!wlan0Exists()` path: 30s confirm + reboot instead of give-up |
| `cmd/sms-gateway/wifi_watchdog.go` | Removed early-exit after 5 failures — existence check continues |

### Status
✅ Fixed — 2026-04-11. New binary deployed same day.

---

## Bug 20: Dashboard Flashes "Not Registered / No Operator" When SMS Arrives (Fixed — 2026-04-11)

### Symptom
Immediately after receiving a text message, the dashboard briefly showed the
gateway as "not registered" with no operator name. The display recovered on the
next 30-second signal poll.

### Root cause
`GetNetworkInfo()` in `internal/atcmd/session.go` always started with a blank
`NetworkInfo{}` struct and **unconditionally wrote it to the cache at the end**,
regardless of whether each AT command succeeded.

The failure sequence:
1. SMS arrives → SMS poller holds the AT mutex (`AT+CMGL`, `AT+CMGD`, SMTP forward)
2. 30-second signal poll fires concurrently and calls `GetNetworkInfo()`
3. `AT+CREG?` response is garbled by RILD noise from the ongoing SMS processing
   → regex fails to match → `Registered` stays `false`
4. `AT+COPS?` likewise → `Operator` stays `""`
5. Cache overwritten with blank state → dashboard shows "not registered, no operator"
6. Next 30-second poll succeeds cleanly → display recovers

The signal bars appeared fine throughout because `GetSignal()` (`AT+CSQ`) is a
simpler command less susceptible to the same RILD interference window.

### Fix
`GetNetworkInfo()` now seeds `info` from the **current cached values** before
sending any AT commands. Each field is only updated when the corresponding
command succeeds *and* the response parses cleanly. Transient failures (timeout,
garbled response, RILD noise) leave the previously-known-good state intact.

A genuine deregistration (`+CREG: 0` parsing cleanly) still updates the cache
correctly — this only suppresses updates where the command failed or the
response was unparseable.

### Files changed
| File | Change |
|------|--------|
| `internal/atcmd/session.go` | `GetNetworkInfo()`: seed from cache; only update fields on clean parse |

### Status
✅ Fixed — 2026-04-11. New binary deployed same day.

---

## Bug 20: Balance Checker Sends "No Reply" Email Even After Response Received (Fixed — 2026-04-12)

### Symptom
Weekly GiffGaff balance check receives a reply (visible in the received SMS
list and the balance response email is sent), but ~10 minutes later a second
email arrives saying "no response received".

### Root cause
The `runBalanceChecker` goroutine stores a local `waitDeadline` variable (set
to 10 minutes after enqueueing the balance check). When the SMS poller
goroutine receives the reply and calls `handleIncomingBalanceResponse`, it
correctly clears `balance_check_pending_since` in the database and sends the
response email. But **it does not reset the balance checker's local
`waitDeadline`**.

The balance checker only resets `waitDeadline` when the timeout fires (line 60).
So even though the reply was processed at 09:00:46, the local `waitDeadline`
remained set to ~09:10:22. At the next 1-minute tick past that deadline, the
checker saw `now.After(waitDeadline)`, skipped the DB flag check entirely, and
sent the spurious "no reply" email.

Log evidence from 2026-04-12:
```
09:00:22 Balance checker: sending INFO to 85075
09:00:36 SMS sent to 85075 (ref=28)
09:00:46 Imported SMS from giffgaff (SIM index 0)
09:00:46 Balance checker: response received from "giffgaff"   ← handled OK
09:00:47 Imported SMS from giffgaff (SIM index 1)
09:00:48 Forwarded service message 62 from giffgaff to admin
09:11:22 Balance checker: no response from GiffGaff within 10 minutes  ← BUG
```

### Fix
Before sending the "no reply" email, the balance checker now re-checks
`balance_check_pending_since` in the database. If it's already been cleared by
`handleIncomingBalanceResponse`, the goroutine resets its local `waitDeadline`
and skips the email:

```go
if !waitDeadline.IsZero() && now.After(waitDeadline) {
    pendingSince, _ := db.GetHealth("balance_check_pending_since")
    if pendingSince == "" {
        // Response was already handled by the poller goroutine.
        waitDeadline = time.Time{}
        continue
    }
    // ... proceed with timeout email
}
```

### Files changed
| File | Change |
|------|--------|
| `cmd/sms-gateway/balance_checker.go` | Added DB flag re-check before timeout email; clears `waitDeadline` on stale deadline |

### Status
✅ Fixed — 2026-04-12.

---

## Feature Notes (not bugs, but non-obvious discoveries)

### GiffGaff Named Sender Encoding
**Discovery**: When GiffGaff responds to "INFO" sent to 85075, the modem
represents their alphanumeric sender ID "giffgaff" as a concatenation of the
decimal ASCII codes for each character: `10310510210210397102102`
(103=g, 105=i, 102=f, 102=f, 103=g, 97=a, 102=f, 102=f).

This sender does NOT start with "+" so `isServiceSender()` correctly identifies
it as a short code / named sender, enabling balance response detection.

GiffGaff typically sends two SMS in response to "INFO": one with credit balance,
one with a link to their app/website. Only the first triggers the admin email
(which clears the pending flag); the second is forwarded as a normal received SMS.

**Relevant code**: `cmd/sms-gateway/balance_checker.go` → `isServiceSender()`
