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
CONFIG_JSON=$GW_DIR/config.json

# ── 0. Exclusive lock ─────────────────────────────────────────────────────────
# PID file guard: prevents two copies of start.sh running simultaneously.
# We check whether the recorded PID is still alive AND that it's actually our
# start.sh (not an unrelated process that got the same PID after a reboot).
# This protects against the case where a reboot kills start.sh without running
# the trap cleanup — the stale PID may be reused by an unrelated process on
# the next boot, which would incorrectly block the gateway from starting.
if [ -f "$PIDFILE" ]; then
    OLD_PID=$(busybox cat "$PIDFILE" 2>/dev/null)
    if [ -n "$OLD_PID" ] && busybox kill -0 "$OLD_PID" 2>/dev/null; then
        # Verify the process is actually start.sh, not a PID-reuse collision.
        CMDLINE=$(busybox cat /proc/$OLD_PID/cmdline 2>/dev/null | busybox tr '\000' ' ')
        if echo "$CMDLINE" | busybox grep -q 'start.sh'; then
            echo "[$(date)] start.sh: already running (PID $OLD_PID) — exiting" >> "$LOG"
            exit 1
        fi
        # PID reuse: stale PID file — proceed and overwrite.
        echo "[$(date)] start.sh: stale PID file (PID $OLD_PID is not start.sh) — clearing" >> "$LOG"
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
# Check force_ap_mode in config.json. When set to true, skip WiFi client setup
# entirely and go straight to AP mode. Used for testing the AP fallback.
FORCE_AP=$(busybox grep -o '"force_ap_mode"[[:space:]]*:[[:space:]]*true' "$CONFIG_JSON" 2>/dev/null)
SETUP_MODE=0

if [ -n "$FORCE_AP" ]; then
    echo "[$(date)] start.sh: force_ap_mode set in config — going straight to AP mode" >> "$LOG"
    SETUP_MODE=1
elif busybox ifconfig wlan0 2>/dev/null | busybox grep -q 'inet addr'; then
    # a) wlan0 already has an IP — already in client mode, skip everything.
    echo "[$(date)] start.sh: WiFi already in client mode, skipping setup" >> "$LOG"
elif [ -d /sys/class/net/wlan0 ]; then
    # b) wlan0 exists but no IP — try a soft reconnect before resorting to rmmod/insmod.
    #    This handles Android cgroup teardown killing wpa_supplicant when the init
    #    service is restarted. Avoids an unnecessary rmmod/insmod which wears out
    #    the pronto_wlan driver over time.
    echo "[$(date)] start.sh: wlan0 has no IP, trying soft reconnect..." >> "$LOG"
    busybox killall wpa_supplicant 2>/dev/null
    sleep 2
    busybox ifconfig wlan0 up 2>/dev/null
    sleep 1
    busybox rm -f /data/misc/wifi/sockets/wlan0
    /system/bin/wpa_supplicant -i wlan0 -D nl80211 \
        -c /data/misc/wifi/wpa_supplicant.conf \
        -O /data/misc/wifi/sockets -B >> "$LOG" 2>&1
    sleep 12
    busybox udhcpc -i wlan0 -q -n \
        -s $GW_DIR/scripts/udhcpc.sh \
        -x hostname:sms-gateway >> "$LOG" 2>&1
    if busybox ifconfig wlan0 2>/dev/null | busybox grep -q 'inet addr'; then
        echo "[$(date)] start.sh: soft reconnect succeeded" >> "$LOG"
    else
        # Soft reconnect failed. Fall back to full wifi-setup with driver reload.
        echo "[$(date)] start.sh: soft reconnect failed — running full wifi-setup..." >> "$LOG"
        /system/bin/sh $GW_DIR/scripts/wifi-setup.sh >> "$LOG" 2>&1
    fi
else
    # c) wlan0 device missing — need a full driver reload (AP mode active, or
    #    first boot, or wpa_supplicant.conf not yet written).
    echo "[$(date)] start.sh: wlan0 missing — running full wifi-setup..." >> "$LOG"
    /system/bin/sh $GW_DIR/scripts/wifi-setup.sh >> "$LOG" 2>&1
fi

# Check if WiFi client mode succeeded (only when not already in AP mode).
# If wlan0 still has no IP after all setup attempts, fall back to AP mode.
if [ $SETUP_MODE -eq 0 ]; then
    if busybox ifconfig wlan0 2>/dev/null | busybox grep -q 'inet addr'; then
        echo "[$(date)] start.sh: WiFi client mode OK" >> "$LOG"
    else
        echo "[$(date)] start.sh: WiFi client failed — falling back to AP mode" >> "$LOG"
        SETUP_MODE=1
    fi
fi

# Start AP infrastructure if needed (driver reload, hostapd, dnsmasq, iptables).
if [ $SETUP_MODE -eq 1 ]; then
    /system/bin/sh $GW_DIR/scripts/wifi-ap-start.sh >> "$LOG" 2>&1
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
# In setup mode, sms-gateway --setup-mode runs instead (captive portal only,
# no modem). It exits only if there's an unexpected error; the reboot triggered
# by "Save & Reboot" kills the process cleanly before the device restarts.
while true; do
    if [ $SETUP_MODE -eq 1 ]; then
        echo "[$(date)] start.sh: starting sms-gateway --setup-mode" >> "$LOG"
        "$GW_BIN" --setup-mode >> "$LOG" 2>&1
        echo "[$(date)] start.sh: sms-gateway --setup-mode exited ($?), restarting in 10s" >> "$LOG"
    else
        echo "[$(date)] start.sh: starting sms-gateway" >> "$LOG"
        "$GW_BIN" >> "$LOG" 2>&1
        echo "[$(date)] start.sh: sms-gateway exited ($?), restarting in 10s" >> "$LOG"
    fi
    sleep 10
done
