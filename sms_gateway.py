#!/usr/bin/env python3
"""
ZTE UFI103 SMS Gateway
Sends and receives SMS via ADB using AT commands on /dev/smd11.

Prerequisites (run ONCE per device boot):
  sudo adb shell "/data/local/tmp/cow"   # re-run dirty cow to patch librank
  sudo python3 sms_gateway.py status     # verify

Usage:
  python3 sms_gateway.py status          # check device + signal
  python3 sms_gateway.py inbox           # list all stored SMS
  python3 sms_gateway.py send +447xxx "message"   # send SMS
  python3 sms_gateway.py watch           # poll for incoming SMS (Ctrl-C to stop)
  python3 sms_gateway.py watch --webhook http://localhost:8080/sms  # POST to webhook

The dirty cow exploit MUST be run after each device reboot to patch librank.
After that this script handles everything via ADB + AT commands.
"""

import subprocess
import sys
import time
import json
import os
import argparse
import datetime

ADB = "adb"
ROOTSHELL = "/system/xbin/librank"
BUSYBOX = "/system/bin/busybox"
SMD = "/dev/smd11"
POLL_INTERVAL = 10  # seconds between inbox checks


# ── low-level helpers ────────────────────────────────────────────────────────

def adb_shell(cmd, timeout=30):
    """Run a command on the device via ADB. Returns (stdout, returncode)."""
    result = subprocess.run(
        ["sudo", ADB, "shell", cmd],
        capture_output=True, text=True, timeout=timeout
    )
    return result.stdout.strip(), result.returncode


def root_shell(cmd, timeout=30):
    """Run a command as root via librank (dirty cow root)."""
    return adb_shell(f"{ROOTSHELL} {cmd}", timeout=timeout)


def device_connected():
    result = subprocess.run(
        ["sudo", ADB, "devices"], capture_output=True, text=True, timeout=10
    )
    return "device" in result.stdout and "19ce8266" in result.stdout


def ensure_root():
    out, _ = root_shell("/system/bin/id")
    if "uid=0" not in out:
        raise RuntimeError(
            "Root not available. Run: sudo adb shell /data/local/tmp/cow"
        )


# ── AT command layer ─────────────────────────────────────────────────────────

def push_at_script(script_content, remote_path):
    """Write a script to a local temp file and push to device."""
    import tempfile
    with tempfile.NamedTemporaryFile(mode='w', suffix='.sh', delete=False) as f:
        f.write(script_content)
        local = f.name
    subprocess.run(
        ["sudo", ADB, "push", local, remote_path],
        capture_output=True, check=True
    )
    os.unlink(local)


def run_at_script(script_content, timeout=30):
    """Push a busybox sh script to device and run it as root. Returns output."""
    remote = "/data/local/tmp/_at_tmp.sh"
    push_at_script(script_content, remote)
    out, _ = root_shell(f"{BUSYBOX} sh {remote}", timeout=timeout)
    return out


def at_session(commands_and_waits):
    """
    Run a sequence of AT commands in one persistent smd11 session.
    commands_and_waits: list of (command_str, sleep_seconds)
    Returns raw output.
    """
    script = "#!/system/bin/busybox sh\nexec 3<>" + SMD + "\n"
    for cmd, wait in commands_and_waits:
        # Escape single quotes in command
        safe_cmd = cmd.replace("'", "'\\''")
        script += f"printf '%s\\r\\n' '{safe_cmd}' >&3\n"
        script += f"sleep {wait}\n"
        script += f"busybox dd if={SMD} bs=4096 count=1 2>/dev/null\n"
    script += "exec 3>&-\n"
    return run_at_script(script, timeout=max(60, sum(w for _, w in commands_and_waits) + 15))


# ── SMS operations ────────────────────────────────────────────────────────────

def get_status():
    """Return device status dict."""
    out = at_session([
        ("AT+CSQ", 1),
        ("AT+CREG?", 1),
        ("AT+COPS?", 1),
        ("AT+CIMI", 1),
    ])
    status = {}
    for line in out.splitlines():
        if "+CSQ:" in line:
            parts = line.split(":")[1].strip().split(",")
            rssi = int(parts[0])
            status["signal_rssi"] = rssi
            status["signal_dbm"] = -113 + rssi * 2 if rssi < 99 else None
            status["signal_bars"] = min(5, rssi // 6) if rssi < 99 else 0
        elif "+CREG:" in line:
            code = int(line.split(",")[-1].strip())
            status["registered"] = code in (1, 5)
            status["roaming"] = code == 5
        elif "+COPS:" in line:
            import re
            m = re.search(r'"([^"]+)"', line)
            status["operator"] = m.group(1) if m else "unknown"
        elif line.strip().isdigit() and len(line.strip()) == 15:
            status["imsi"] = line.strip()
    return status


def list_sms(storage="SM"):
    """
    List all SMS from specified storage (SM=SIM, ME=modem memory, MT=both).
    Returns list of dicts with keys: index, status, sender, timestamp, text
    """
    out = at_session([
        (f'AT+CPMS="{storage}","{storage}","{storage}"', 1),
        ("AT+CMGF=1", 1),
        ('AT+CSCS="IRA"', 1),
        ('AT+CMGL="ALL"', 3),
    ])

    messages = []
    lines = out.splitlines()
    i = 0
    while i < len(lines):
        line = lines[i].strip()
        if line.startswith("+CMGL:"):
            # Parse header: +CMGL: index,"status","sender",,"timestamp"
            import re
            m = re.match(
                r'\+CMGL:\s*(\d+),"([^"]*)",'  # index, status
                r'"([^"]*)",.*?,"([^"]*)"',      # sender, timestamp
                line
            )
            if m:
                idx, status, sender, ts = m.groups()
                # Next non-empty line is the message text (modem inserts blank line)
                text = ""
                for j in range(i + 1, min(i + 5, len(lines))):
                    candidate = lines[j].strip()
                    if candidate and not candidate.startswith("+CMGL:") and candidate != "OK":
                        text = candidate
                        break
                # Decode hex if needed (some modems return hex-encoded text)
                if text and all(c in "0123456789ABCDEFabcdef" for c in text) and len(text) % 2 == 0 and len(text) > 10:
                    try:
                        text = bytes.fromhex(text).decode("latin-1")
                    except Exception:
                        pass
                messages.append({
                    "index": int(idx),
                    "status": status,
                    "sender": sender,
                    "timestamp": ts,
                    "text": text,
                    "storage": storage,
                })
                i += 2
                continue
        i += 1
    return messages


def send_sms(number, text):
    """
    Send an SMS via AT+CMGS.
    Returns True on success, raises on failure.
    """
    # The AT+CMGS flow:
    # 1. Send AT+CMGS="<number>\r"  → device responds with "> "
    # 2. Send <message_text>\x1a    → device responds with +CMGS: <ref> or ERROR
    # We handle this in a single busybox sh script with timed reads.
    remote = "/data/local/tmp/_at_send.sh"
    script = f"""#!/system/bin/busybox sh
DEV={SMD}
exec 3<>$DEV
printf 'AT+CMGF=1\\r\\n' >&3; sleep 1
busybox dd if=$DEV bs=256 count=1 2>/dev/null
printf 'AT+CSCS="IRA"\\r\\n' >&3; sleep 1
busybox dd if=$DEV bs=256 count=1 2>/dev/null
printf 'AT+CMGS="{number}"\\r' >&3
sleep 1.5
busybox dd if=$DEV bs=256 count=1 2>/dev/null
printf '{text}\\x1a' >&3
sleep 6
busybox dd if=$DEV bs=4096 count=1 2>/dev/null
exec 3>&-
"""
    push_at_script(script, remote)
    out, _ = root_shell(f"{BUSYBOX} sh {remote}", timeout=30)
    if "+CMGS:" in out:
        import re
        m = re.search(r'\+CMGS:\s*(\d+)', out)
        ref = m.group(1) if m else "?"
        return True, f"Sent (ref={ref})"
    elif "ERROR" in out:
        return False, f"Modem error: {out}"
    else:
        return False, f"Unexpected response: {out}"


def get_inbox_via_db():
    """
    Read SMS database directly from /data/data/com.android.providers.telephony/databases/mmssms.db
    The content://sms provider requires READ_SMS permission which even root can't satisfy
    via binder, but we can read the underlying SQLite file directly as root.
    Returns list of dicts.
    """
    import sqlite3, tempfile, os
    remote_db = "/data/data/com.android.providers.telephony/databases/mmssms.db"
    tmp_db = "/data/local/tmp/mmssms_tmp.db"
    local_db = tempfile.mktemp(suffix=".db")
    try:
        root_shell(f"/system/bin/cp {remote_db} {tmp_db} && /system/bin/chmod 644 {tmp_db}")
        subprocess.run(["sudo", ADB, "pull", tmp_db, local_db],
                       capture_output=True, check=True)
        db = sqlite3.connect(local_db)
        rows = db.execute(
            "SELECT _id, address, date, body, type, read FROM sms ORDER BY date DESC"
        ).fetchall()
        db.close()
        messages = []
        for r in rows:
            messages.append({
                "id": str(r[0]),
                "sender": r[1] or "",
                "timestamp_ms": r[2],
                "timestamp": datetime.datetime.fromtimestamp(r[2] / 1000).strftime("%Y-%m-%d %H:%M:%S"),
                "text": r[3] or "",
                "type": r[4],  # 1=inbox, 2=sent
                "read": bool(r[5]),
            })
        return messages
    except Exception as e:
        return []
    finally:
        try:
            os.unlink(local_db)
        except Exception:
            pass


# ── watch mode ────────────────────────────────────────────────────────────────

def watch_inbox(webhook_url=None):
    """
    Watch for incoming SMS using AT+CNMI unsolicited result codes via smd11.
    When +CMTI: "SM",<idx> arrives, reads the message at that index and reports it.
    Falls back to polling AT+CMGL every POLL_INTERVAL seconds as a safety net.
    """
    import urllib.request
    import tempfile, os

    print("Watching for incoming SMS via AT+CNMI...")
    print("Press Ctrl-C to stop.\n")

    # Seed known indices so we don't re-report existing messages
    known_indices = set()
    last_count = 0
    try:
        existing = list_sms("SM")
        known_indices = {m["index"] for m in existing}
        last_count = len(existing)
        print(f"Baseline: {last_count} existing SIM messages (indices: {sorted(known_indices)})")
    except Exception as e:
        print(f"Warning: could not seed baseline: {e}")

    def read_and_report(idx):
        """Read a specific SIM index and report/webhook the message."""
        try:
            script = f"""#!/system/bin/busybox sh
exec 3<>/dev/smd11
printf 'AT+CPMS="SM","SM","SM"\\r\\n' >&3; sleep 1
busybox dd if=/dev/smd11 bs=512 count=1 2>/dev/null
printf 'AT+CMGF=1\\r\\n' >&3; sleep 1
busybox dd if=/dev/smd11 bs=512 count=1 2>/dev/null
printf 'AT+CSCS="IRA"\\r\\n' >&3; sleep 1
busybox dd if=/dev/smd11 bs=512 count=1 2>/dev/null
printf 'AT+CMGR={idx}\\r\\n' >&3; sleep 2
busybox dd if=/dev/smd11 bs=4096 count=1 2>/dev/null
exec 3>&-
"""
            out = run_at_script(script, timeout=20)
            import re
            m = re.search(
                r'\+CMGR:\s*"([^"]*)",'  # status
                r'"([^"]*)",'             # sender
                r'.*?"([^"]*)"'           # timestamp
                r'\s*\n+\s*(.*?)(?:\s*OK|\s*$)',  # body
                out, re.DOTALL
            )
            if m:
                status, sender, ts, body = m.groups()
                body = body.strip()
                # Decode hex if needed
                if body and all(c in "0123456789ABCDEFabcdef" for c in body) and len(body) % 2 == 0 and len(body) > 10:
                    try:
                        body = bytes.fromhex(body).decode("latin-1")
                    except Exception:
                        pass
                msg = {
                    "index": idx,
                    "status": status,
                    "sender": sender,
                    "timestamp": ts,
                    "text": body,
                    "storage": "SM",
                }
                print(f"\n[{ts}] FROM {sender} (SIM index {idx})")
                print(f"  {body}")
                if webhook_url:
                    try:
                        payload = json.dumps(msg).encode()
                        req = urllib.request.Request(
                            webhook_url,
                            data=payload,
                            headers={"Content-Type": "application/json"},
                            method="POST"
                        )
                        urllib.request.urlopen(req, timeout=5)
                        print(f"  [webhook OK]")
                    except Exception as we:
                        print(f"  [webhook error: {we}]")
        except Exception as e:
            print(f"  [error reading index {idx}: {e}]")

    import re
    print(f"Polling SIM count every {POLL_INTERVAL}s for new messages...\n")
    try:
        while True:
            try:
                # Quick check: get current SIM message count via AT+CPMS?
                count_out = at_session([
                    ('AT+CPMS="SM","SM","SM"', 1),
                    ("AT+CPMS?", 1),
                ])
                m = re.search(r'\+CPMS:\s*"SM",(\d+)', count_out)
                if not m:
                    time.sleep(POLL_INTERVAL)
                    continue
                current_count = int(m.group(1))

                if current_count > last_count:
                    print(f"Count increased {last_count} → {current_count}, reading new messages...")
                    # Read all messages and report new ones
                    all_msgs = list_sms("SM")
                    for msg in all_msgs:
                        if msg["index"] not in known_indices:
                            known_indices.add(msg["index"])
                            print(f"\n[{msg['timestamp']}] FROM {msg['sender']}")
                            print(f"  {msg['text']}")
                            if webhook_url:
                                try:
                                    payload = json.dumps(msg).encode()
                                    req = urllib.request.Request(
                                        webhook_url, data=payload,
                                        headers={"Content-Type": "application/json"},
                                        method="POST"
                                    )
                                    urllib.request.urlopen(req, timeout=5)
                                    print(f"  [webhook OK]")
                                except Exception as we:
                                    print(f"  [webhook error: {we}]")
                    last_count = current_count
                else:
                    print(f"  [{time.strftime('%H:%M:%S')}] {current_count} messages, no change")

            except Exception as e:
                print(f"Poll error: {e}")
            time.sleep(POLL_INTERVAL)
    except KeyboardInterrupt:
        print("\nStopped.")


# ── CLI ───────────────────────────────────────────────────────────────────────

def cmd_status(args):
    if not device_connected():
        print("ERROR: Device not connected via ADB.")
        sys.exit(1)
    try:
        ensure_root()
    except RuntimeError as e:
        print(f"ERROR: {e}")
        sys.exit(1)

    status = get_status()
    print("=== ZTE UFI103 Status ===")
    print(f"  Operator:   {status.get('operator', '?')}")
    print(f"  Registered: {status.get('registered', '?')}  Roaming: {status.get('roaming', '?')}")
    sig = status.get('signal_dbm')
    bars = status.get('signal_bars', 0)
    rssi = status.get('signal_rssi', 0)
    print(f"  Signal:     {sig} dBm  (RSSI={rssi}, {'▓' * bars}{'░' * (5-bars)})")
    print(f"  IMSI:       {status.get('imsi', '?')}")


def cmd_inbox(args):
    if not device_connected():
        print("ERROR: Device not connected via ADB.")
        sys.exit(1)
    ensure_root()

    storage = getattr(args, 'storage', 'SM')
    print(f"Reading SMS from {storage} storage...")
    messages = list_sms(storage)
    if not messages:
        print("  No messages found.")
        return
    print(f"\n{len(messages)} message(s):\n")
    for msg in messages:
        print(f"[{msg['index']}] {msg['status']} | From: {msg['sender']} | {msg['timestamp']}")
        print(f"    {msg['text']}")
        print()


def cmd_send(args):
    if not device_connected():
        print("ERROR: Device not connected via ADB.")
        sys.exit(1)
    ensure_root()

    number = args.number
    text = args.message
    if not number.startswith("+"):
        print("WARNING: number should be in international format (+44xxx)")
    print(f"Sending to {number}: {text!r}")
    ok, msg = send_sms(number, text)
    if ok:
        print(f"  SUCCESS: {msg}")
    else:
        print(f"  FAILED: {msg}")
        sys.exit(1)


def cmd_watch(args):
    if not device_connected():
        print("ERROR: Device not connected via ADB.")
        sys.exit(1)
    webhook = getattr(args, 'webhook', None)
    watch_inbox(webhook_url=webhook)


def main():
    parser = argparse.ArgumentParser(
        description="ZTE UFI103 SMS Gateway via ADB + AT commands"
    )
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("status", help="Show device status and signal")

    p_inbox = sub.add_parser("inbox", help="List stored SMS")
    p_inbox.add_argument("--storage", default="SM",
                         choices=["SM", "ME", "MT"],
                         help="Storage: SM=SIM, ME=modem, MT=both (default: SM)")

    p_send = sub.add_parser("send", help="Send an SMS")
    p_send.add_argument("number", help="Destination number (e.g. +447700000000)")
    p_send.add_argument("message", help="Message text")

    p_watch = sub.add_parser("watch", help="Watch for incoming SMS")
    p_watch.add_argument("--webhook", default=None,
                         help="HTTP endpoint to POST new messages to (JSON)")

    args = parser.parse_args()

    dispatch = {
        "status": cmd_status,
        "inbox": cmd_inbox,
        "send": cmd_send,
        "watch": cmd_watch,
    }
    dispatch[args.command](args)


if __name__ == "__main__":
    main()
