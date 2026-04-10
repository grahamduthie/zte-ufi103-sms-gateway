# ZTE UFI103 SMS Gateway

A self-contained SMS↔email bridge running on a ZTE UFI103 4G dongle (Qualcomm MSM8916, Android 4.4). Texts sent to the dongle's SIM are forwarded as HTML emails. Email replies are sent back as SMS. No host PC needed after initial setup.

## Quick Start

```bash
cd /home/marlowfm/dongle/sms-gateway

# Build
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o sms-gateway ./cmd/sms-gateway

# Deploy
adb push sms-gateway /data/sms-gateway/sms-gateway.new
adb shell "/system/xbin/librank /system/bin/busybox kill \
  \$(busybox ps | busybox awk '/sms-gateway$/{print \$1}')"
sleep 2
adb shell "/system/xbin/librank /system/bin/busybox mv \
  /data/sms-gateway/sms-gateway.new /data/sms-gateway/sms-gateway"
```

**Web UI**: `http://172.16.10.226/` (password is set in `config.json` → `web.admin_password`)

## Features

- **SMS → Email**: Incoming texts are polled from the SIM every 2 seconds, stored in SQLite, and forwarded as styled HTML emails via SMTP
- **Email → SMS**: IMAP IDLE monitors for replies; authorised senders can text back by replying to the forwarded email
- **Conversation view**: Threaded chat-bubble UI for each contact, paginated
- **Multi-network WiFi**: Three networks configured with priority fallback (home, alternative location, third location)
- **Boot persistence**: Gateway auto-starts on boot via Android init
- **Web UI**: Dashboard, Received, Sent, Conversations, Compose, Settings — all behind a password gate
- **WiFi watchdog**: Automatically reconnects if the WiFi drops, without reloading the driver module
- **Delivery confirmations**: Email sent when an SMS reply is delivered or fails permanently

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  sms-gateway (Go binary, /data/sms-gateway/sms-gateway)     │
│                                                               │
│  7 Goroutines:                                               │
│  1. SMS poller    → AT+CPMS? every 2s → imports new SMS     │
│  2. Send queue    → drains pending SMS every 10s            │
│  3. IMAP IDLE     → persistent connection, instant delivery │
│  4. Signal poller → AT+CSQ/COPS every 30s (web UI)        │
│  5. Web server    → HTTP on :80                              │
│  6. WiFi watchdog → checks wlan0 IP every 45s, reconnects  │
│  7. Housekeeping  → hourly log rotation, WAL, pruning      │
└─────────────────────────────────────────────────────────────┘
         │                              │
         ▼                              ▼
  /dev/smd11 (AT commands)     Ionos email servers
  persistent fd +              YOUR_IMAP_HOST:993 (TLS)
  background reader            YOUR_SMTP_HOST:587 (STARTTLS)
                               IMAP IDLE: persistent connection
```

## Documentation

| I want to... | Read... |
|-------------|---------|
| Understand the system architecture | [GATEWAY.md](GATEWAY.md) |
| See the current status and quick reference | [STATUS.md](STATUS.md) |
| Understand the hardware, root, and AT commands | [DEVICE.md](DEVICE.md) |
| See bug history and root causes | [BUGS.md](BUGS.md) |
| Understand CNMI/QMI SMS routing | [SMS_MODEM_ARCHITECTURE.md](SMS_MODEM_ARCHITECTURE.md) |
| See completed refactoring items | [REFACTOR_PLAN.md](REFACTOR_PLAN.md) |
| Run tests and add new ones | [FULL_PROJECT_TEST_PLAN.md](FULL_PROJECT_TEST_PLAN.md) |
| See the WiFi AP fallback plan | [WIFI_AP_PLAN.md](WIFI_AP_PLAN.md) |
| See the documentation roadmap | [DOCUMENTATION_PLAN.md](DOCUMENTATION_PLAN.md) |

## Key Files

```
sms-gateway/
├── cmd/sms-gateway/
│   ├── main.go            # Daemon entry point, goroutine setup
│   ├── housekeeping.go    # Log rotation, WAL checkpoint, record pruning
│   └── wifi_watchdog.go   # Soft WiFi reconnect (no rmmod/insmod)
├── internal/
│   ├── atcmd/session.go   # AT commands, persistent reader, PDU SMS send
│   ├── atcmd/pdu.go       # GSM 7-bit PDU encoding
│   ├── config/config.go   # JSON config loading + validation
│   ├── database/db.go     # SQLite operations
│   ├── email/bridge.go    # SMTP forward + IMAP IDLE reply processing
│   └── web/server.go      # HTTP routes + embedded HTML templates
└── scripts/
    ├── start.sh           # Init entry point: PID guard + WiFi setup + restart loop
    ├── wifi-setup.sh      # WiFi AP→client mode switch (multi-network)
    └── udhcpc.sh          # DHCP event script
```

## Testing

130 automated tests across 5 packages:

```bash
cd /home/marlowfm/dongle/sms-gateway
go test ./...           # All tests
go test ./... -race     # Race detector
go test ./... -v        # Verbose
```

## Hardware

| Property | Value |
|----------|-------|
| Model | ZTE UFI103 (ZX_UFI103) |
| Chipset | Qualcomm MSM8916 |
| CPU | Quad-core ARM Cortex-A53 (32-bit) |
| RAM | 512MB |
| Android | 4.4.4 (KitKat) |
| WiFi | 2.4GHz only |

## License

Private project — all rights reserved.
