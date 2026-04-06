#!/system/bin/sh
# SMS Gateway startup script — runs as an Android init service (sms-gw).
#
# Triggered by sys.boot_completed=1 via /init.target.rc.
# Also works when called manually for development:
#   /system/xbin/librank /system/bin/sh /data/sms-gateway/start.sh
#
# Flow:
#   0. Exclusive lock — abort if another instance is already running
#   1. Brief settling sleep (modem already up at boot_completed)
#   2. Switch WiFi to client mode (skip if already connected)
#   3. Rotate log if >5 MB
#   4. Run sms-gateway in a crash-restart loop (foreground — init manages lifetime)

GW_DIR=/data/sms-gateway
GW_BIN=$GW_DIR/sms-gateway
LOG=$GW_DIR/sms-gateway.log
PIDFILE=$GW_DIR/gateway.pid

# ── 0. Exclusive lock ─────────────────────────────────────────────────────────
# PID file guard: prevents two copies of start.sh running simultaneously,
# regardless of how the second copy is launched.
# We check whether the recorded PID is still alive (kill -0 does not send a
# signal; it just tests whether the process exists). If the old PID is gone
# (crash/kill/reboot), we replace the PID file and continue normally.
if [ -f "$PIDFILE" ]; then
    OLD_PID=$(busybox cat "$PIDFILE" 2>/dev/null)
    if [ -n "$OLD_PID" ] && busybox kill -0 "$OLD_PID" 2>/dev/null; then
        echo "[$(date)] start.sh: already running (PID $OLD_PID) — exiting" >> "$LOG"
        exit 1
    fi
fi
echo $$ > "$PIDFILE"
trap "busybox rm -f '$PIDFILE'" EXIT INT TERM HUP

# ── 1. Settling sleep ─────────────────────────────────────────────────────────
# At boot_completed, RILD and the modem have been up for ~60s. We only need
# a short pause here; the WiFi setup below adds another ~25s before the gateway
# actually opens /dev/smd11.
sleep 5

# ── 2. WiFi client mode ───────────────────────────────────────────────────────
# Skip if wlan0 already has an IP (e.g., on a manual restart mid-session).
if busybox ifconfig wlan0 2>/dev/null | busybox grep -q 'inet addr'; then
    echo "[$(date)] start.sh: WiFi already in client mode, skipping setup" >> "$LOG"
else
    echo "[$(date)] start.sh: setting up WiFi client mode..." >> "$LOG"
    /system/bin/sh $GW_DIR/scripts/wifi-setup.sh >> "$LOG" 2>&1
fi

# ── 3. Log rotation ───────────────────────────────────────────────────────────
if [ -f "$LOG" ]; then
    SIZE=$(busybox stat -c%s "$LOG" 2>/dev/null || echo 0)
    if [ "$SIZE" -gt 5242880 ]; then
        busybox mv "$LOG" "$LOG.1"
    fi
fi

# ── 3b. Remove stock ZTE port 80 redirect ────────────────────────────────────
# The stock firmware adds an iptables DNAT rule that redirects all port 80
# traffic to port 8000 (the stock web UI). Remove it so our gateway can
# serve on port 80 (standard HTTP).
iptables -t nat -D oem_nat_pre -p tcp -d 192.168.100.1 --dport 80 -j DNAT --to-destination 192.168.100.1:8000 2>/dev/null

# ── 4. Crash-restart loop ─────────────────────────────────────────────────────
# The gateway runs in the foreground. If it exits for any reason, we restart it
# after a 10s delay. Because this script is the init service's main process
# (not a background child), init never tears down the cgroup on us.
while true; do
    echo "[$(date)] start.sh: starting sms-gateway" >> "$LOG"
    "$GW_BIN" >> "$LOG" 2>&1
    echo "[$(date)] start.sh: sms-gateway exited ($?), restarting in 10s" >> "$LOG"
    sleep 10
done
