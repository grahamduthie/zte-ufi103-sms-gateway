# SMS Gateway — Refactoring Plan for Rock-Solid Operation
## 2026-04-04

The gateway works end-to-end but has structural fragility. This plan addresses
every crash, leak, race, and silent failure identified by deep code review.

---

## Priority 1: Prevent Crashes (Do This First)

### 1.1 Fix `respBuf` Unbounded Growth
**Problem**: The persistent reader's `respBuf` (`strings.Builder`) grows forever.
Every byte RILD sends (AT+CPMS? every 3-5s) is appended. After hours/days this
will OOM the device (512MB RAM, ~25MB budget for the daemon).

**Fix**: Implement a ring buffer with bounded capacity.
```go
const maxRespBufSize = 64 * 1024 // 64KB ring buffer

type Session struct {
    respMu    sync.Mutex
    respBuf   []byte    // ring buffer
    respStart int       // logical start index
    respEnd   int       // logical end index
}
```
When writing exceeds capacity, advance `respStart` (oldest data is lost).
Callers track positions relative to `respStart` instead of absolute indices.

**Alternative (simpler)**: Truncate the buffer after every command completes.
Since all callers use position-based slicing, we can truncate from position 0
after every `sendCommand`/`sendCommandsMulti` returns. This means only ONE
in-flight command's responses are kept at a time.

### 1.2 Fix `readerLoop` Cannot Update `s.reader` After Reopen
**Problem**: `readerLoop` captures `s.reader` in its closure. When `reopen()`
creates a new `bufio.Reader`, the goroutine still uses the old broken one.
This means **any modem reset permanently breaks the daemon** until restart.

**Fix**: Don't use `bufio.Reader`. Read directly from `s.file` using `Read()`
in the loop, building lines from a byte buffer. After reopen, `s.file` is
updated and the next `Read()` uses the new fd.

```go
func (s *Session) readerLoop() {
    defer close(s.readerDone)
    var line []byte
    for {
        b := make([]byte, 1)
        n, err := s.file.Read(b)
        if err != nil {
            // handle reopen/close
            continue
        }
        if n > 0 {
            if b[0] == '\n' {
                s.respMu.Lock()
                s.respBuf.Write(line)
                s.respBuf.WriteByte('\n')
                s.respMu.Unlock()
                line = line[:0]
            } else {
                line = append(line, b[0])
            }
        }
    }
}
```

### 1.3 Fix `sendSMSDirectAT` Writes to Potentially Stale fd
**Problem**: `sendSMSDirectAT` holds `s.mu` but `s.file` is not protected
by that mutex. If `reopen()` fires concurrently, `s.file` could be closed
while `sendSMSDirectAT` is writing to it.

**Fix**: Make `s.file` access use its own mutex (`fdMu`). Or better:
don't share `s.file` across goroutines at all — open a dedicated fd
for SMS sends (see §2.1).

### 1.4 Add Panic Recovery to ALL Goroutines
**Problem**: If any goroutine panics, it dies silently. Only the main
process's `signal.Ignore(SIGHUP)` survives.

**Fix**: Every goroutine launched in `main.go` gets a `defer recover()`
wrapper with logging. Already partially done but needs to be in EVERY
goroutine including the signal poller and the IMAP poller's inner loops.

---

## Priority 2: Architectural Fixes

### 2.1 Separate SMS Send Path from Polling Path
**Problem**: `SendSMS` uses the same `Session` (and same `s.file`) as
the SMS poller. When a send takes 35 seconds, it blocks ALL other AT
commands (polling, signal checks, network info).

**Fix**: Create a dedicated `SendSession` that opens its own fd for
the duration of a send, then closes it. The poller keeps using the
persistent `Session` for reads. They serialize via the AT mutex but
don't share the same fd.

```go
type Gateway struct {
    pollSession *atcmd.Session    // persistent fd for polling
    sendMu      sync.Mutex        // serialize sends
}

func (g *Gateway) SendSMS(number, text string) (int, error) {
    g.sendMu.Lock()
    defer g.sendMu.Unlock()
    
    // Open dedicated fd for this send
    sess, err := atcmd.NewSession("/dev/smd11")
    if err != nil { return 0, err }
    defer sess.Close()
    
    return sess.sendSMSDirectAT(number, text)
}
```

### 2.2 Circuit Breaker for AT Commands
**Problem**: If the modem gets into a bad state (e.g., SIM re-locks,
PDP context drops, RILD restarts), the poller hammers it every 2 seconds
with commands that all fail, burning CPU and never recovering.

**Fix**: Implement a circuit breaker that backs off exponentially:
```
fail 1: retry in 2s
fail 2: retry in 4s
fail 3: retry in 8s
...
fail 6: retry in 60s (max)
success: reset counter
```

After 10 consecutive failures, mark the session as "unhealthy" and
attempt a soft reset (close fd, reopen, unlock SIM).

### 2.3 Fix `parseCMGL` Message Skipping
**Problem**: The double `i++` (for loop + manual) assumes every SMS
has exactly one text line after the header. If a message has an empty
body, the next message's `+CMGL:` header is skipped entirely.

**Fix**: Don't manually increment `i`. Instead, detect `+CMGL:` headers
and process each one:
```go
for i := 0; i < len(lines); i++ {
    line := strings.TrimSpace(lines[i])
    if !strings.HasPrefix(line, "+CMGL:") {
        continue
    }
    // parse header
    // collect subsequent non-header lines as body
    var bodyLines []string
    for j := i + 1; j < len(lines); j++ {
        next := strings.TrimSpace(lines[j])
        if next == "" || strings.HasPrefix(next, "+CMGL:") || next == "OK" {
            break
        }
        bodyLines = append(bodyLines, next)
        i = j  // advance to last body line
    }
    msgs = append(msgs, SMS{..., Text: strings.Join(bodyLines, "\n")})
}
```

### 2.4 Fix `decodeIfNeeded` False-Positive Hex Decoding
**Problem**: Any even-length string of 10+ hex chars is decoded as
GSM 7-bit packed data, corrupting legitimate text like "Balance: 00AABB".

**Fix**: After decoding, verify the output is printable ASCII. If not,
return the original text:
```go
decoded := gsm7Decode(bytes)
for _, r := range decoded {
    if r < 32 && r != '\n' && r != '\r' && r != '\t' {
        return text  // not printable — keep original
    }
}
return decoded
```

---

## Priority 3: Robustness Improvements

### 3.1 Database Connection Pool Configuration
```go
db.SetMaxOpenConns(1)   // SQLite can only handle one writer
db.SetMaxIdleConns(1)
db.SetConnMaxLifetime(0) // don't expire connections
```

### 3.2 Config Validation at Startup
```go
func (c *Config) Validate() error {
    if c.Email.SMTPHost == "" { return fmt.Errorf("smtp_host required") }
    if c.Email.Username == "" { return fmt.Errorf("email username required") }
    if c.Email.Password == "" { return fmt.Errorf("email password required") }
    if c.Email.ForwardTo == "" { return fmt.Errorf("forward_to required") }
    if len(c.AuthorisedSenders) == 0 { return fmt.Errorf("authorised_senders required") }
    if c.SMS.PollIntervalSec < 1 { return fmt.Errorf("poll interval must be >= 1") }
    return nil
}
```

### 3.3 Send Queue Retry Limit and Backoff
Currently: retries 10 times with no delay between attempts.
Fix: exponential backoff (10s, 20s, 40s, ... up to 5min) with a max
of 50 total attempts. After 50, mark as permanently failed and send
a delivery confirmation email.

### 3.4 Health Endpoint Enrichment
Add to `/status`:
```json
{
  "uptime_seconds": 3600,
  "poll_failures": 0,
  "last_poll_time": "2026-04-04T15:41:00Z",
  "last_send_time": "2026-04-04T14:20:24Z",
  "last_imap_time": "2026-04-04T14:17:07Z",
  "circuit_breaker_state": "closed",
  "resp_buf_size_bytes": 4096
}
```

### 3.5 Log Rotation
Currently: log file grows unbounded.
Fix: Truncate log file when it exceeds 5MB, keeping the last 100 lines.

### 3.6 Graceful Shutdown
Currently: `at.Close()` is called but goroutines aren't signaled to stop.
Fix: Use a `context.Context` with cancel. On SIGINT/SIGTERM, cancel the
context, wait for goroutines to finish (with 10s timeout), then exit.

---

## Priority 4: Edge Cases

### 4.1 Empty SMS Body Handling
Some messages arrive with no body (delivery reports, flash SMS). The
`parseCMGL` fix (§2.3) handles this, but we should also skip importing
messages with empty bodies to avoid noise.

### 4.2 SIM Full (50 Messages)
Add monitoring: if SIM count > 40, log a warning and send an email alert.
The current code deletes after forward, so this should never happen, but
if forwarding fails for an extended period the SIM will fill.

### 4.3 Network Drop (WiFi Disconnects)
The IMAP poller will time out (60s deadline) and log an error. The SMTP
forwarder will fail similarly. These are already handled gracefully
(errors logged, health status updated). No code changes needed.

### 4.4 Duplicate SMS from SMSC Retry
If the SMSC retries a message that was already delivered but not yet
deleted from the SIM, we'd import it twice. The `MessageExistsBySIMIndex`
check prevents this (after the sim_index NULL fix).

### 4.5 Concurrent Database Access
SQLite via `modernc.org/sqlite` is thread-safe but serializes all writes.
With `SetMaxOpenConns(1)`, concurrent reads are fine and writes queue up.
No code changes needed beyond the pool config.

---

## Priority 5: IMAP IDLE (Next Up)

### 5.1 Replace IMAP Poll with Persistent IDLE Connection
**Problem**: IMAP is polled every 60 seconds. Email-to-SMS replies take up to
60 seconds to be delivered. Reducing the interval increases server load; a
persistent poll-less approach is better.

**Solution**: Use the IMAP IDLE extension (RFC 2177). The client keeps a
persistent TLS connection to the Ionos server. When a new message arrives, the
server sends an unsolicited `* N EXISTS` response, waking the daemon
immediately. go-imap v1.2.1 has built-in IDLE support in `client.Client.Idle()`.

**API** (go-imap v1.2.1):
```go
// IdleOptions controls IDLE behaviour.
type IdleOptions struct {
    LogoutTimeout time.Duration  // how often to restart IDLE (default 25min)
    PollInterval  time.Duration  // fallback poll interval if server doesn't support IDLE
}

// Idle enters IDLE mode. Blocks until stop is closed or an error occurs.
// Unsolicited updates (EXISTS, EXPUNGE) arrive on c.Updates.
func (c *Client) Idle(stop <-chan struct{}, opts *IdleOptions) error
```

**Updates channel**: Set `c.Updates = make(chan client.Update, 4)` before
dialling. The channel receives `*client.MailboxUpdate` when the message count
changes. Check `mbu.Mailbox.Messages` to know if there are new messages.

**Implementation plan**:

1. **Add `IdleLoop(ctx context.Context)` to `internal/email/bridge.go`**

   ```go
   func (b *Bridge) IdleLoop(ctx context.Context) {
       backoff := 5 * time.Second
       for {
           err := b.runIdleSession(ctx)
           if ctx.Err() != nil {
               return  // clean shutdown
           }
           b.log.Printf("IMAP IDLE: reconnecting in %v (err: %v)", backoff, err)
           select {
           case <-time.After(backoff):
               if backoff < 5*time.Minute { backoff *= 2 }
           case <-ctx.Done():
               return
           }
           backoff = 5 * time.Second  // reset on successful reconnect
       }
   }
   ```

2. **Add `runIdleSession(ctx context.Context) error`**

   ```go
   func (b *Bridge) runIdleSession(ctx context.Context) error {
       // 1. Dial + TLS (same as PollReplies, but no per-op deadline on rawConn)
       // 2. Create updates channel: updates := make(chan client.Update, 4)
       // 3. c.Updates = updates   ← must set BEFORE client.New()... actually
       //    set after: c.Updates = updates
       // 4. Login, Select("INBOX", false)
       // 5. Call b.fetchAndProcessUnseen(c) to handle any already-waiting mail
       // 6. Loop:
       //      stop := make(chan struct{})
       //      idleDone := make(chan error, 1)
       //      go func() { idleDone <- c.Idle(stop, &IdleOptions{LogoutTimeout: 25*time.Minute}) }()
       //      select {
       //      case update := <-updates:
       //          if mbu, ok := update.(*client.MailboxUpdate); ok && mbu has new messages {
       //              close(stop); <-idleDone
       //              b.fetchAndProcessUnseen(c)
       //          }
       //      case err := <-idleDone:
       //          return err  // unexpected IDLE error → reconnect
       //      case <-ctx.Done():
       //          close(stop); <-idleDone; return nil
       //      }
   }
   ```

   **Important**: After `close(stop)` and IDLE exits, the connection is back
   in SELECTED state and SEARCH/FETCH can be issued directly without
   re-selecting INBOX.

3. **Extract `fetchAndProcessUnseen(c *client.Client) error`**

   Factored out of `PollReplies()` — does SEARCH UNSEEN → FETCH → `processReply`
   for each result. `PollReplies()` becomes a thin wrapper that dials + calls
   this helper (keep it for the `--test` mode).

4. **Health/status updates inside `IdleLoop`**: set `imap_status = "idle:ok"`,
   `last_imap_time` each time a wake-up fetch completes. On reconnect error,
   set `imap_status = "idle:reconnecting"`.

5. **Update `main.go`**: Replace the 60-second IMAP ticker goroutine with:
   ```go
   wg.Add(1)
   go func() {
       defer wg.Done()
       defer func() {
           if r := recover(); r != nil {
               logger.Printf("IMAP IDLE recovered from panic: %v", r)
           }
       }()
       bridge.IdleLoop(ctx)
   }()
   ```
   Remove `imapPollInterval` variable; `imap_poll_interval_seconds` in config
   can stay for the fallback path in `IdleOptions.PollInterval` (set it to
   `60*time.Second` so behaviour degrades gracefully if Ionos doesn't support IDLE).

**Gotchas**:
- The raw connection must NOT have a short deadline set (unlike the current 60s
  deadline in `PollReplies`). Use `rawConn.SetDeadline(time.Time{})` (zero =
  no deadline) after dialling — the `Idle()` call may block for 25 minutes.
- `c.Updates` must be a buffered channel (capacity ≥ 1) or assigning it blocks.
- On reconnect, always call `fetchAndProcessUnseen` before entering IDLE again
  — messages may have arrived during the disconnected window.
- Keep `PollReplies()` working (used by `--test` flag).

---

## Implementation Status (2026-04-04)

| Step | Task | Status |
|------|------|--------|
| 1 | respBuf truncation (not ring buffer — simpler) | ✅ Done |
| 2 | readerLoop byte-by-byte read (no bufio) | ✅ Done |
| 3 | Panic recovery in all goroutines | ✅ Already done |
| 4 | parseCMGL rewrite | ✅ Done |
| 5 | decodeIfNeeded output validation | ✅ Done |
| 6 | Dedicated SendSession for SMS sends | ❌ Not feasible — hardware limitation (see BUGS.md) |
| 7 | Circuit breaker for AT commands | ✅ Done |
| 8 | Config validation | ✅ Done |
| 9 | Database pool config | ✅ Done |
| 10 | Send queue backoff (max 50 attempts, exp backoff) | ✅ Done |
| 11 | Health endpoint enrichment | ✅ Done |
| 12 | Log rotation | ✅ Done 2026-04-05 — hourly housekeeping goroutine rotates at 10MB; start.sh rotates at 5MB on startup |
| 13 | Graceful shutdown | ✅ Done |
| 14 | IMAP IDLE persistent connection | ✅ Done 2026-04-04 — uses `go-imap-idle`, 25min keepalive, auto-reconnect with exponential backoff |
| 15 | SIM proactive unlock in SMS poller | ✅ Done 2026-04-04 — `EnsureUnlocked()` called at start of every poll, before any SMS operations |
| 16 | Boot persistence via init wrapper | ✅ Done 2026-04-05 — sms-gw named service in /init.target.rc; start.sh runs gateway in foreground so cgroup is never torn down |
| 17 | Shell injection fix in send_shell.go | ✅ Done 2026-04-05 — deprecated, added input validation (phone number regex, text metachar rejection) |
| 18 | Input validation in sendSMSDirectAT | ✅ Done 2026-04-05 — rejects empty number, empty text, text >160 chars |
| 19 | decodeQuotedPrintable multi-byte UTF-8 fix | ✅ Done 2026-04-05 — ParseInt→ParseUint, byte accumulation for UTF-8 |
| 20 | Automated test suite (130 tests) | ✅ Done 2026-04-05 — config(13), database(25), email(25), atcmd(25), web(15); grown to 130 by 2026-04-06 |
| 21 | Nil AT session handling in web server | ✅ Done 2026-04-05 — handleDashboard and handleStatus check for nil |
| 22 | Database integrity check | ✅ Done 2026-04-05 — CheckIntegrity() via PRAGMA integrity_check |
| 23 | Send queue visibility | ✅ Done 2026-04-05 — GetSendQueueStats() returns pending/failed/sent counts |
| 24 | deploy.sh test gate | ✅ Done 2026-04-05 — runs go test before build |
| 25 | Single-instance guard in start.sh | ✅ Done 2026-04-05 — PID file approach (busybox flock -n fd NOT supported on BusyBox v1.23) |
| 26 | WiFi watchdog goroutine | ✅ Done 2026-04-05 — soft reconnect every 45s (wpa_supplicant restart + udhcpc); never does rmmod/insmod |
| 27 | WAL checkpoint + record pruning | ✅ Done 2026-04-05 — housekeeping.go: PRAGMA wal_checkpoint(TRUNCATE) + DELETE WHERE older than 90 days |

---

## What NOT to Change

- The persistent reader architecture for polling — it works, buffer management
  fixes are in place
- The IMAP/SMTP email bridge — already robust with timeouts and error handling
- The Web UI templates — functional and stable
- The SIM PIN unlock mechanism — works correctly
- The PDU mode SMS send with `promptCh` — do not revert to text mode or remove
  the `promptCh` mechanism. See Bug 13 in `BUGS.md` and `SMS_MODEM_ARCHITECTURE.md`
  for why this is critical.

## Additional Items Completed (2026-04-06)

| # | Task | Status |
|---|------|--------|
| 36 | Bug 13: RILD injection in SMS send | ✅ Fixed — PDU mode (`AT+CMGF=0`), `readerLoop` flushes `>` immediately + signals `promptCh`, PDU written in microseconds before RILD reacts |
| 37 | AT+CNMI moved from every-poll to startup + hourly | ✅ Fixed — was triggering RILD AT+CPMS on every 2s poll cycle |
| 38 | Git repository initialised, pushed to GitHub | ✅ Done — https://github.com/grahamduthie/zte-ufi103-sms-gateway (private) |

---

*Plan created: 2026-04-04*
*Device: ZTE UFI103, Serial 19ce8266*
*Status: Gateway functional but fragile — this plan makes it production-ready*

---


---

*See also: `STATUS.md` (current status), `GATEWAY.md` (architecture), `BUGS.md` (bug history), `WIFI_AP_PLAN.md` (next: WiFi AP fallback with captive portal), `FULL_PROJECT_TEST_PLAN.md` (comprehensive test plan — 111 tests passing), `DOCUMENTATION_PLAN.md` (documentation roadmap)*

---

## Additional Items Completed (2026-04-05 Evening)

| # | Task | Status |
|---|------|--------|
| 28 | Web UI port 80 + password gate | ✅ Gateway on port 80, password `mfm`, all routes protected |
| 29 | Conversation list + thread view | ✅ Paginated list (30/page), chat-bubble threads, mobile responsive |
| 30 | GetSMSCount RILD interleaving fix | ✅ Isolates our AT+CPMS? response from RILD's interleaved response |
| 31 | Dashboard overhaul | ✅ Monthly counts, UK-time last sent/received, gateway status, uptime |
| 32 | Settings Danger Zone | ✅ Restart Gateway and Reboot Dongle buttons |
| 33 | Conversation pagination + indexes | ✅ `idx_messages_sender_time` and `idx_queue_tonumber_time` |
| 34 | Nav reorganisation | ✅ Conversations between Sent and Compose, Inbox→Received |
| 35 | Logo update | ✅ Square mfm_logo.png at 48px in top bar |

---

## Additional Items Completed (2026-04-09)

| # | Task | Status |
|---|------|--------|
| 36 | Conversation thread order fixed | ✅ Messages now oldest-first (chronological), not reverse |
| 37 | Delivery confirmation email threading | ✅ Subject matches original SMS: `Re: Text from +44... [DDMMYY-NNN]` |
| 38 | Email template cleanup | ✅ Removed Reference row from forwarded SMS, Modem Ref from delivery emails |
| 39 | Multi-network WiFi | ✅ YOUR_WIFI_SSID_1, YOUR_WIFI_SSID_2, YOUR_WIFI_SSID_3 — priority-ordered fallback |
| 40 | Settings page poll interval min fix | ✅ HTML `min=1` matches Go validation `>= 1` (was `min=5`) |
| 41 | Shut Down Dongle button | ✅ Clean power-off via `sys.powerctl shutdown` |
| 42 | /restarting page | ✅ Spinner page polls /status, auto-redirects when gateway is back |
| 43 | Boot persistence mechanism clarified | ✅ `qrngp` wrapper (not named init service) — see Bug 14 in BUGS.md |
| 44 | Settings save auto-restart reverted | ✅ Save just persists config; use Restart Gateway in Danger Zone |
