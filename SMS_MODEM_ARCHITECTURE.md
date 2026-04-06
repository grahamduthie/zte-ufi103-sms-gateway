# SMS Modem Architecture — Discovery Notes for Future LLMs

*Written 2026-04-06 after live debugging. Updated 2026-04-06 to add outbound
SMS architecture after Bug 13 fix. Intended to prevent future misdiagnosis of
both inbound and outbound SMS issues on this device.*

---

## The Problem That Prompted This Document

Inbound SMS were not reaching the gateway. `GetSMSCount` was returning 0
without errors. Direct AT diagnostics (`AT+CPMS?`, `AT+CMGL="ALL"`) confirmed
the SIM storage was genuinely empty — texts were never arriving there. Yet
outbound SMS (email→SMS) and IMAP IDLE both worked perfectly.

---

## Two Competing SMS Paths on This Device

The ZTE UFI103 (Qualcomm MSM8916, Android 4.4) has two completely separate
routes by which incoming SMS can be handled:

### Path 1 — QMI WMS (RILD's private pipe)

RILD (the Android Radio Interface Layer Daemon) communicates with the modem
over `/dev/smd36` using a Qualcomm binary protocol called **QMI WMS** (Wireless
Message Service). This is RILD's *primary* and preferred SMS channel. When a
text arrives, the modem can deliver it to RILD via a QMI indication, completely
bypassing the AT command interface.

**Key property**: SMS delivered via this path are NOT written to SIM (SM)
storage. They exist only in RILD's process memory and are forwarded to the
Android telephony framework (or, on this device, discarded — because
`com.qualcomm.telephony` replaces the standard stack and `mmssms.db` is always
empty).

### Path 2 — AT command storage (our gateway's path)

The modem can also store incoming SMS in SIM card storage (the "SM" memory
store), making them readable via AT commands: `AT+CPMS?` (count),
`AT+CMGL="ALL"` (list), `AT+CMGR=N` (read by index). Our gateway polls this
storage every 2 seconds.

**Key property**: SMS only appear here if the modem is explicitly configured
to write them here. If not configured, they flow via QMI only and this storage
stays permanently empty.

---

## The Switch That Controls Which Path is Used: AT+CNMI

`AT+CNMI` is an AT command that configures **New Message Indication** behaviour.
Its second parameter (`mt`, for Mobile Terminated messages) is the critical one:

| `mt` value | Behaviour |
|-----------|-----------|
| `0` | No notification. Modem routes SMS via QMI only. **SM storage is not written.** |
| `1` | Modem writes SMS to preferred storage (SM, as set by `AT+CPMS`), then sends a `+CMTI` unsolicited result code: e.g. `+CMTI: "SM",0` (new message at SIM slot 0). |
| `2` | Modem delivers SMS body directly as a `+CMT` unsolicited result code, without storing it anywhere. |

The full command format is `AT+CNMI=<mode>,<mt>,<bm>,<ds>,<bfr>`. We use
`AT+CNMI=2,1,0,0,0`.

### What RILD does at boot

RILD issues `AT+CNMI=0,0,0,0,0` during its initialisation sequence. It does
this because it receives SMS via QMI and has no use for AT-based notifications.
Setting `mt=0` keeps the shared `/dev/smd11` channel free of unsolicited noise.

**This is what broke our gateway.** With `mt=0`, every incoming text was
silently consumed by RILD's QMI path. Our gateway polled an SM storage that
was never written to.

### Diagnostic signature of this failure

- `sms_status: "ok (count=0)"` — no parse errors, count is genuinely 0
- `AT+CPMS?` and `AT+CMGL="ALL"` (run while gateway is stopped) both confirm
  SM storage is empty
- Outbound SMS, IMAP IDLE, WiFi all work normally
- The failure is *consistent*, not intermittent

---

## The Fix Applied

`AT+CNMI=2,1,0,0,0` is sent **once at startup** in `main.go` (via
`at.SetTextMode()`) and re-applied **hourly** in `housekeeping.go` as a safety
net. It is **NOT** sent on every poll cycle — see the critical warning below.

With `mt=1` active:

1. When a text arrives, the modem writes it to SM storage (slot 0, 1, 2 …)
2. The modem sends `+CMTI: "SM",N` on `/dev/smd11`
3. Our `readerLoop` in `session.go` detects the `+CMTI:` prefix and signals
   `NewMessageCh` (a buffered channel)
4. The SMS poller goroutine in `main.go` selects on `NewMessageCh` and calls
   `processSMS` immediately, without waiting for the next 2-second tick
5. `GetSMSCount` sees count > 0; `ListSMS` reads the message; it is inserted
   into the database, emailed, and deleted from SIM

Observed end-to-end latency after the fix: under 5 seconds from text sent to
email forwarded.

---

## CRITICAL: Do NOT Send AT+CNMI on Every Poll

Sending `AT+CNMI=2,1,0,0,0` in the `GetSMSCount` command sequence (which runs
every 2 seconds) was tried and **causes outbound SMS to fail reliably**. Here's
why:

RILD reacts to `AT+CNMI=2,1,0,0,0` by immediately sending
`AT+CPMS="SM","SM","SM"` on the shared channel. If this reaction arrives while
the modem is in SMS text-input mode (i.e. after `AT+CMGS=N` sends `>`), the
modem receives RILD's bytes as part of the PDU body → `+CMS ERROR: 304`.

**Current architecture** (as of 2026-04-06):
- `AT+CNMI=2,1,0,0,0` sent once at startup in `main.go` via `SetTextMode()`
- Re-applied every hour in `housekeeping.go` as a safety net
- `GetSMSCount` does NOT include AT+CNMI

The risk of hourly re-application is: if RILD resets `mt=0` between hourly
cycles, a text arriving in that window is silently lost. There is no evidence
RILD does this periodically (it only resets at boot), so hourly is acceptable.

## RILD's AT+CPMS Behaviour

RILD reacts to certain AT commands by sending `AT+CPMS="SM","SM","SM"` to
reassert its preferred storage. The trigger commands are:

| Our command | RILD reaction | Timing |
|-------------|---------------|--------|
| `AT+CNMI=...` | `AT+CPMS="SM","SM","SM"` | ~50ms after our command |
| `AT+CMGF=0` | `AT+CPMS="SM","SM","SM"` | ~50–100ms after our command |

RILD also issues `AT+CPMS?` (query, NOT set) every ~3–5 seconds independently.
These query responses appear in our read buffer but do not change any settings.

**Why this matters for SMS sending**: anything RILD writes to `/dev/smd11`
while the modem is in text-input mode (after `AT+CMGS=N` and before `Ctrl-Z`)
is treated by the modem as SMS body text. This was Bug 13.

---

## RILD's Periodic AT+CPMS? Queries

RILD issues `AT+CPMS?` (storage *query*) every 3–5 seconds on its own timer.
Its response (`+CPMS: "SM",0,20,...`) appears in our gateway's read buffer
interleaved with our own responses. This was the subject of Bug 11 (see
`BUGS.md`) — the gateway was misidentifying RILD's count=0 response as its own.

RILD also issues `AT+CPMS="SM","SM","SM"` (storage *set*) as a reactive
response to certain AT commands we send (see the table above). Do not confuse
these two: the periodic query is harmless; the reactive set is the Bug 13
injection hazard.

---

---

## Outbound SMS: How to Send Correctly

This section documents the correct, battle-tested approach for sending an SMS
on this device. Do not deviate from it without understanding every point below.

### Use PDU Mode, Not Text Mode

**Text mode** (`AT+CMGF=1`): The modem treats everything it receives as raw SMS
body text until `Ctrl-Z`. Any bytes arriving from any source (including RILD)
are silently included in the message. There is no way to detect corruption — the
message is sent containing RILD's AT commands with no error.

**PDU mode** (`AT+CMGF=0`): The modem expects strict hexadecimal. If RILD
injects non-hex bytes, the modem returns `+CMS ERROR: 304` — a clean failure
with no garbled SMS sent. The message is retried rather than corrupted.

**Always use PDU mode. Never use text mode for sending.**

### The `>` Prompt Is NOT a Newline-Terminated Line

The modem response to `AT+CMGS=N` is `\r\n> ` — a carriage return, newline,
`>`, and space, with **no trailing newline**. This is critical.

`readerLoop` accumulates bytes into a local `line` buffer and only flushes to
`respBuf` when it sees `\n`. Therefore:

- The `\r\n` before `>` causes an empty line to flush.
- The `>` and space accumulate in `line` **but are never flushed** (no `\n`).
- `>` does NOT appear in `respBuf` through normal operation.

The `readerLoop` was extended (2026-04-06) to flush immediately when it reads
a `>` byte, and to signal `promptCh` (a dedicated `chan struct{}`) at that
instant. `sendSMSDirectAT` waits on `promptCh` via `select`. This means the
PDU is written within **microseconds** of the modem sending `>`.

**If you ever rewrite `readerLoop` or `sendSMSDirectAT`, preserve this
behaviour. Without it, `>` is undetectable and every send will fail.**

### Why Speed Matters: The RILD Race

When the modem enters text-input mode and sends `>`, RILD sees the channel
activity from its own file descriptor and reacts by writing
`AT+CPMS="SM","SM","SM"\r\n` approximately 2ms later. If our PDU write is
slower than RILD's reaction, RILD's bytes arrive at the modem first and corrupt
the PDU.

The `promptCh` mechanism ensures our PDU write happens in microseconds —
well inside RILD's ~2ms reaction time.

### The Complete Working Send Sequence

```
[acquire s.mu]

1. ESC (0x1B)             — cancel any stuck text-input mode from a previous
                            failed attempt
   sleep 100ms
   truncate respBuf

2. AT+CMGF=0\r\n          — set PDU mode
                            RILD reacts: AT+CPMS="SM","SM","SM" arrives ~50ms later
   wait-for-quiet:         poll respBuf until it stops growing for 250ms
                            (or 3s max). This flushes RILD's reaction.
   truncate respBuf        — clean baseline for the AT+CMGS exchange

3. drain promptCh          — discard any stale signal from a previous attempt

4. record cmgsStart = len(respBuf)
   AT+CMGS=<tpduLen>\r    — tpduLen = TPDU octet count (NOT counting "00" SMSC prefix)

5. select on promptCh      — readerLoop signals this the instant it reads '>'
   (or 5s timeout fallback)

6. On '>' detected:
   write "00" + tpduHex + 0x1A   — "00" = use SIM's stored SMSC
                                   tpduHex = GSM7/UCS2 encoded PDU (uppercase hex)
                                   0x1A = Ctrl-Z = send

7. Wait up to 35s for response:
   +CMGS: <ref>           → success, return ref
   \r\nOK\r\n             → success (some firmware omits ref), return 0
   +CMS ERROR             → RILD injection or other failure, return error (retry)
   timeout                → return error (retry)

[release s.mu]
```

### PDU Format

The PDU written in step 6 is:
- `"00"` — SMSC length of 0, meaning "use SIM's stored SMSC" (always use this)
- TPDU hex — the TP layer PDU as uppercase hexadecimal

The `AT+CMGS=N` parameter `N` is the **TPDU octet count only** — it does NOT
count the `"00"` SMSC prefix. `N = len(tpduHex) / 2`.

The TPDU is built by `encodeSMSPDU()` in `internal/atcmd/pdu.go`. It uses
GSM 7-bit encoding for ASCII-range text and UCS-2 for anything requiring it.
Maximum 160 GSM7 characters or 70 UCS-2 characters per message.

### What NOT to Do

| Don't | Why |
|-------|-----|
| Use `AT+CMGF=1` (text mode) for sending | RILD injection is silent — garbled SMS sent with no error |
| Put `AT+CNMI=2,1,0,0,0` in the GetSMSCount sequence | Triggers RILD AT+CPMS reaction every 2s, reliably hitting every send window |
| Poll for `>` with a sleep loop | `>` is never in `respBuf` without `promptCh`; you'd only detect it after RILD injects (already corrupt) |
| Remove the quiet-wait after `AT+CMGF=0` | RILD's AT+CPMS reaction arrives ~50ms after CMGF=0 and will corrupt the next PDU |
| Open a new fd per-send | Both fds share the same kernel SMD channel; RILD writes to both |
| Try AT+CMGW/AT+CMSS (write-then-send) | Also uses text-input mode; same RILD injection problem |

---

## Summary for Future Debugging

### Inbound SMS not arriving

1. Check `sms_status` in `/status`. If `"ok (count=0)"` with no errors → SM
   storage is empty → suspect `AT+CNMI` has been reset to `mt=0`.
2. Kill the gateway, run `AT+CNMI?` via a raw AT session. If it shows
   `+CNMI: 0,0,0,0,0`, RILD reset it at boot (normal) and the gateway hasn't
   re-applied it yet. Restart the gateway — `SetTextMode()` at startup applies
   `AT+CNMI=2,1,0,0,0`.
3. If the gateway is running and still `mt=0`, the binary may be old or
   `SetTextMode()` failed — check the log for a warning at startup.

### Outbound SMS failing with +CMS ERROR: 304

This is RILD injection. The modem received non-hex bytes in the PDU body.
Possible causes:

- `promptCh` mechanism was broken or removed — `>` was not detected fast enough.
- `AT+CNMI=2,1,0,0,0` was added back to the poll cycle — triggers RILD AT+CPMS
  every 2 seconds, reliably hitting every send window.
- The quiet-wait after `AT+CMGF=0` was removed — RILD's reaction wasn't flushed.

Check `internal/atcmd/session.go`: `readerLoop` must flush on `>` and signal
`promptCh`; `sendSMSDirectAT` must wait on `promptCh` before writing the PDU.

### Outbound SMS showing AT commands in the received text

This means text mode (`AT+CMGF=1`) was used. Switch to PDU mode (`AT+CMGF=0`)
with `encodeSMSPDU()`. In text mode there is no way to prevent RILD corruption.

---

*See also: `BUGS.md` (Bug 11, Bug 13), `GATEWAY.md` (architecture), `DEVICE.md`
(RILD and AT command details)*
