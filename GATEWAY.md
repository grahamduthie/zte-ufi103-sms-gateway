# SMS Gateway — Architecture & Configuration

## Overview

A single statically-linked ARM Go binary (`sms-gateway`) runs entirely on the
ZTE UFI103 dongle. It handles SMS receive, SMS send, email forwarding, email
reply processing, and a web UI. No host PC needed after initial setup.

## Architecture

```
┌───────────────────────────────────────────────────────────────────┐
│  sms-gateway (Go binary, /data/sms-gateway/sms-gateway)           │
│                                                                    │
│  Goroutines:                                                       │
│  1. SMS poller      → AT+CPMS? every 2s → imports new SMS         │
│  2. Send queue      → drains pending SMS every 10s                │
│  3. IMAP IDLE       → persistent connection, instant delivery     │
│  4. Signal poller   → AT+CSQ/COPS every 30s (for web UI)         │
│  5. Web server      → HTTP on :80                                 │
│  6. WiFi watchdog   → checks wlan0 IP every 45s, soft-reconnects │
│  7. Housekeeping    → hourly log rotation, WAL checkpoint, pruning│
│                                                                    │
│  All goroutines have defer recover() + logging                     │
│  SIGHUP is ignored so daemon survives adb shell disconnect         │
└───────────────────────────────────────────────────────────────────┘
         │                              │
         ▼                              ▼
  /dev/smd11 (AT commands)     Ionos email servers
  persistent fd +              YOUR_IMAP_HOST:993 (TLS)
  background reader            YOUR_SMTP_HOST:587 (STARTTLS)
                               IMAP IDLE: persistent connection
                               (25min keepalive, auto-reconnect)
```

## SMS Receive Flow

1. `AT+CPMS?` → get message count (every 2 seconds)
2. If count > 0:
   - `AT+CMGL="ALL"` → list all messages
   - For each message not in database: insert into SQLite, `AT+CMGD=index,0` (delete from SIM)
3. Forward unforwarded messages via SMTP

**Key design**: Uses a persistent fd for `/dev/smd11` with a background reader
goroutine. Responses are tracked via position-based slicing to separate our
responses from RILD's interleaved `AT+CPMS?` noise.

### How RILD and the modem handle incoming SMS

*Full research notes are in `SMS_MODEM_ARCHITECTURE.md`. Summary below.*

On this Qualcomm MSM8916 device, RILD uses **QMI WMS** (over `/dev/smd36`,
not AT commands) as its primary SMS channel. RILD sets `AT+CNMI=0,0,0,0,0` at
boot, which with `mt=0` causes the modem to route incoming SMS exclusively via
QMI — bypassing SIM (SM) storage entirely. Our gateway overrides this by
sending `AT+CNMI=2,1,0,0,0` **once at startup** (in `main.go` via
`SetTextMode()`) and re-applying it **hourly** in `housekeeping.go`. It is NOT
sent on every poll — doing so triggers RILD to react with `AT+CPMS="SM","SM","SM"`
on every cycle, which injects bytes into the SMS send window (see Bug 13,
`BUGS.md`). With `mt=1`, incoming SMS are written to SM storage AND the modem
sends a `+CMTI` unsolicited result code.

`+CMTI:` notifications DO reach our `/dev/smd11` fd when `mt=1` is active. The
`readerLoop` detects them and signals `NewMessageCh`, triggering an immediate
poll rather than waiting up to 2 seconds for the next tick.

RILD does NOT delete messages from SM storage — they persist until our gateway
reads and deletes them with `AT+CMGD`.

The Android telephony database (`mmssms.db` at
`/data/data/com.android.providers.telephony/databases/mmssms.db`) is
**completely empty** on this device — `com.qualcomm.telephony` (a Qualcomm
replacement for the standard Android telephony stack) handles SMS at the QMI
layer without writing to the standard database. The `pollAndroidSMS()` fallback
in `android_sms.go` is harmless dead code on this device.

### Why messages were missed (historical)

The root cause of missed messages during early development was **two gateway
instances running simultaneously**. Both opened `/dev/smd11` (the SMD channel
allows multiple opens) and competed for AT command responses — each process
would steal the other's modem replies. The fix is the PID file guard in
`start.sh` (see Boot section below).

## SMS Send Flow

Implemented in `sendSMSDirectAT()` in `internal/atcmd/session.go`. Uses PDU
mode throughout — never text mode (see below for why).

1. **ESC** → cancel any stuck text-input state from a previous failed send
2. **`AT+CMGF=0\r\n`** → set PDU mode. RILD reacts by sending
   `AT+CPMS="SM","SM","SM"`. Wait for channel to go quiet (buffer stops
   growing for 250ms), then truncate the buffer.
3. **`AT+CMGS=<tpduLen>\r`** → `tpduLen` is TPDU octet count (NOT counting
   the `"00"` SMSC prefix). Modem responds with `> `.
4. **`>` detected via `promptCh`** → `readerLoop` flushes `>` to `respBuf`
   immediately (without waiting for `\n`) and signals `promptCh`. The send
   function waits on `promptCh` via `select` — zero polling delay, responds
   in microseconds.
5. **`"00" + tpduHex + 0x1A`** → SMSC prefix `"00"` means "use SIM's stored
   SMSC". `tpduHex` is built by `encodeSMSPDU()` in `pdu.go` (GSM 7-bit
   encoding). `0x1A` = Ctrl-Z = send.
6. **Wait up to 35s** for `+CMGS: <ref>` (success), bare `OK` (success,
   no ref), or `+CMS ERROR` (RILD injection or modem error → retry).

**Why PDU mode**: In text mode (`AT+CMGF=1`) RILD's `AT+CPMS` bytes arrive in
the text-input window and are silently included in the message — no error, just
a garbled SMS. In PDU mode (`AT+CMGF=0`) any non-hex injection gives `+CMS
ERROR: 304` — a clean failure that triggers a retry. See Bug 13 in `BUGS.md`
and `SMS_MODEM_ARCHITECTURE.md` for full analysis.

**Why `promptCh` is critical**: The modem's `>` prompt is `\r\n> ` — no
trailing newline. `readerLoop` only flushed to `respBuf` on `\n`, so `>` was
invisible until RILD happened to follow it with a newline-terminated line (i.e.
already injected). `promptCh` solves this with an immediate flush + signal.

## Email Reply Flow (IMAP IDLE → SMS)

1. Persistent TLS connection to `YOUR_IMAP_HOST:993` (`InsecureSkipVerify: true`)
2. **IDLE mode** — server pushes `* N EXISTS` when new mail arrives
3. On wake-up, SEARCH for `UNSEEN` messages
4. For each unseen message:
   - Extract `[SMS xxxxxxxx]` prefix from Subject → session ID
   - Look up original sender number
   - Check `From:` against authorised senders list
   - Extract plain text body, decoding:
     - Quoted-printable encoding (`=E2=80=99` → `'`)
     - Strips "On ... wrote:" blocks, Outlook headers, `> ` quoted lines
     - Strips everything after `\n---` separator
     - Normalises Unicode to GSM-compatible ASCII
   - Truncate to 160 characters
   - Enqueue in `send_queue` table
5. On disconnect: auto-reconnect with exponential backoff (5s → 10s → 20s → … → 5min)

## Boot and Process Management

### Init service

The gateway is launched via the `qrngp` wrapper script (in `/init.target.rc`),
which runs `/data/sms-gateway/start.sh` on `sys.boot_completed=1`. `qrngp` is a
shell wrapper that sleeps 30 seconds then invokes `start.sh` via `librank`.

`start.sh` is the service's **foreground main process** — it never exits
(crash-restart loop), so Android init never tears down the cgroup.

There is also a legacy `userinit.sh` boot hook (at `/data/local/userinit.sh`)
that also tries to start `start.sh`. The PID file guard handles this correctly.

**Warning**: After an abrupt disconnect (e.g. pulling the USB cable), the stale
`gateway.pid` file can prevent the gateway from starting on next boot. Delete
`/data/sms-gateway/gateway.pid` before rebooting if you encounter this, or use
the **Shut Down Dongle** button in the web UI for a clean power-off.

### Single-instance guard (PID file)

`start.sh` writes its own PID to `gateway.pid` on startup and checks for a
live process on the next invocation. If a second copy tries to start (from
init, userinit.sh, manual adb, or any other source), it sees the existing PID
is alive and exits immediately.

**Important**: `gateway.pid` is owned by root:root (mode 600) because the init
service runs as root. It cannot be read from the adb shell (uid=2000) — but the
second invocation of `start.sh` also runs as root and can read it correctly.

**Do not use `busybox flock` with a numeric fd argument on this device.**
BusyBox v1.23 (2014) does not support `flock -n <fd>` — `exec 9>file` opens
the fd but `flock -n 9` fails with "Bad file descriptor", causing every
invocation to exit with "another instance running". Use the PID file approach
instead.

### Deploying a new binary

The running gateway is owned by root (uid=0). The adb shell is uid=2000 and
cannot kill it directly. Use `librank` (the SUID rootshell at
`/system/xbin/librank`) to send the signal:

```bash
# Push new binary to staging area (never overwrite running binary directly)
adb push sms-gateway /data/sms-gateway/sms-gateway.new

# Kill the running gateway (start.sh's crash loop restarts it within 10s)
adb shell "/system/xbin/librank /system/bin/busybox kill \
  \$(busybox ps | busybox awk '/sms-gateway$/{print \$1}')"
sleep 2

# Move new binary into place
adb shell "/system/xbin/librank /system/bin/busybox mv \
  /data/sms-gateway/sms-gateway.new /data/sms-gateway/sms-gateway"
```

## Configuration (`/data/sms-gateway/config.json`)

```json
{
    "sms": {
        "poll_interval_seconds": 2,
        "storage": "SM",
        "delete_after_forward": true,
        "sim_pin": "8837"
    },
    "email": {
        "imap_host": "212.227.24.222",
        "imap_port": 993,
        "smtp_host": "212.227.24.158",
        "smtp_port": 587,
        "username": "YOUR_EMAIL_USERNAME",
        "password": "YOUR_EMAIL_PASSWORD",
        "forward_to": "your-email@example.com",
        "from_name": "Marlow FM SMS"
    },
    "authorised_senders": ["your-email@example.com"],
    "sms_max_reply_chars": 160,
    "imap_poll_interval_seconds": 60,
    "web": {
        "listen_addr": "0.0.0.0:80"
    },
    "database": "/data/sms-gateway/sms.db",
    "log_file": "/data/sms-gateway/sms-gateway.log"
}
```

**Note**: IP addresses are used for email servers because `/etc/resolv.conf`
cannot be created on the read-only ramdisk — the device has no DNS resolver.

**Note**: `imap_poll_interval_seconds` is retained for backwards compatibility;
IMAP uses IDLE (not periodic polling). The log_file path is used by the
housekeeping goroutine for log rotation.

## Database Schema (`sms.db`)

```sql
CREATE TABLE messages (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    sim_index         INTEGER,            -- NULL after deletion from SIM; -2 = Android DB source (unused)
    sender            TEXT NOT NULL,
    received_at       TEXT NOT NULL,
    body              TEXT NOT NULL,
    forwarded_at      TEXT,               -- NULL until emailed
    forward_attempts  INTEGER DEFAULT 0,
    email_session_id  TEXT,
    session_prefix    TEXT,
    deleted_from_sim  INTEGER DEFAULT 0
);

CREATE TABLE email_sessions (
    session_id     TEXT PRIMARY KEY,
    session_prefix TEXT NOT NULL,
    message_id     INTEGER NOT NULL REFERENCES messages(id),
    sender_number  TEXT NOT NULL,
    created_at     TEXT NOT NULL
);

CREATE TABLE send_queue (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    to_number      TEXT NOT NULL,
    body           TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending',
    sent_at        TEXT,
    failure_reason TEXT,
    modem_ref      INTEGER,
    source         TEXT NOT NULL DEFAULT 'email_reply',
    attempts       INTEGER DEFAULT 0,
    next_retry_at  TEXT,
    session_prefix TEXT
);

CREATE TABLE daemon_health (
    key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL
);
```

**daemon_health keys**: `started_at`, `sms_status`, `last_poll_time`,
`circuit_breaker`, `imap_status`, `last_imap_time`, `last_android_sms_id`
(always 0 — Android DB path is unused on this device).

## Housekeeping (hourly goroutine)

`cmd/sms-gateway/housekeeping.go` runs every hour:

1. **Log rotation**: renames `sms-gateway.log` → `sms-gateway.log.1` when the
   file exceeds 10 MB, then creates a fresh empty file. At most 20 MB of logs
   on disk at any time. `start.sh` also rotates at 5 MB on startup.
2. **WAL checkpoint**: `PRAGMA wal_checkpoint(TRUNCATE)` to merge the WAL file
   into the main database and shrink it back to zero bytes.
3. **Record pruning**: deletes `messages` and completed/failed `send_queue`
   entries older than 90 days. Runs `PRAGMA optimize` afterwards.

## Web UI Routes

| Route | Method | Purpose |
|-------|--------|---------|
| `/login` | POST | Password gate (see `web.admin_password` in config.json) |
| `/logout` | POST | Clear auth cookie |
| `/` | GET | Dashboard — monthly counts, last sent/received (UK time), gateway status, uptime, recent messages |
| `/inbox` | GET | Paginated received SMS (20/page) — now labelled "Received" |
| `/sent` | GET | Paginated sent SMS (50/page) |
| `/conversation` | GET | **Conversation list** (30/page with pagination) or single thread with chat bubbles |
| `/compose` | GET/POST | Manual SMS send form |
| `/settings` | GET/POST | Configuration + Danger Zone (Restart Gateway / Reboot Dongle / Shut Down Dongle) |
| `/restarting` | GET | Spinner page — polls `/status`, redirects to `/` when gateway is back |
| `/status` | GET | JSON health endpoint (auth required) |

**Auth**: All routes except `/login`, `/logout`, and `/static/*` require the `gw_auth` cookie (set by logging in with the password from `web.admin_password` in config.json).

## Build Process

```bash
cd /home/marlowfm/dongle/sms-gateway
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o sms-gateway ./cmd/sms-gateway
# deploy.sh wraps this with go test gate + adb push
```

## Key Code Files

```
sms-gateway/
├── cmd/sms-gateway/
│   ├── main.go            # Daemon entry point, goroutine setup
│   ├── android_sms.go     # Fallback: polls mmssms.db (dead code on this device)
│   ├── housekeeping.go    # Log rotation, WAL checkpoint, record pruning
│   └── wifi_watchdog.go   # Soft WiFi reconnect (no rmmod/insmod)
├── internal/
│   ├── atcmd/session.go   # AT commands, persistent reader, +CMTI/promptCh detection, PDU SMS send
│   ├── atcmd/pdu.go       # GSM 7-bit PDU encoding
│   ├── config/config.go   # JSON config loading + validation
│   ├── database/db.go     # SQLite operations
│   ├── email/bridge.go    # SMTP forward + IMAP IDLE reply processing
│   └── web/server.go      # HTTP routes + templates
└── scripts/
    ├── start.sh           # Init service entry point: PID guard + WiFi setup + restart loop
    ├── wifi-setup.sh      # WiFi AP→client mode switch (dynamic IP via udhcpc.sh)
    ├── udhcpc.sh          # DHCP event script — configures wlan0 IP/route/DNS
    └── wifi-client-start.sh  # Manual WiFi switch (dev use)
```

## Dependencies

| Package | Purpose |
|---------|---------|
| `modernc.org/sqlite` | Pure-Go SQLite driver (no CGO) — v1.34.2 |
| `github.com/emersion/go-imap` | IMAP client library |
| `golang.org/x/sys` | Unix syscalls (used by housekeeping) |

## Known Limitations

1. **No DNS resolution** — must use IP addresses for external services
2. **RILD noise** — `AT+CPMS?` polls every 3-5s, interleaved with our responses
   (mitigated: `GetSMSCount` now isolates our response from RILD's)
3. **Shared Session** — poller, sender, and signal poller share one `Session`;
   sends block all polling for up to 35 seconds
4. **`AT+CNMI` override resilience** — RILD sets `mt=0` at boot; we re-apply
   `AT+CNMI=2,1,0,0,0` once at startup and hourly in housekeeping. RILD is not
   observed to reset this periodically (only at boot), so a 60-minute re-apply
   window is acceptable. If RILD ever did reset it mid-hour, a text arriving in
   that window would be missed permanently (see `SMS_MODEM_ARCHITECTURE.md`)
5. **mmssms.db always empty** — Qualcomm's telephony replacement doesn't write
   to the standard Android database; `pollAndroidSMS()` is dead code
6. **WiFi driver instability** — `pronto_wlan.ko` crashes after ~3 wpa_supplicant
   restarts per boot session. `wlan0` disappears and only a reboot recovers it.
   The watchdog has been hardened (exponential backoff, failure limit, missing
   device detection) to avoid making it worse. See Bug 15 in `BUGS.md`.

## Testing

130 automated tests across 5 packages:

```bash
cd /home/marlowfm/dongle/sms-gateway
go test ./...           # All tests
go test ./... -race     # Race detector
go test ./... -v        # Verbose
```

---

*See also: `STATUS.md` (current status), `DEVICE.md` (hardware specs), `BUGS.md` (all bugs and fixes), `SMS_MODEM_ARCHITECTURE.md` (CNMI/QMI SMS routing research), `REFACTOR_PLAN.md` (completed refactoring items), `WIFI_AP_PLAN.md` (planned WiFi AP fallback)*
