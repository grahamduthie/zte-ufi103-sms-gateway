#!/system/bin/sh
# WiFi Client Mode Startup Script — ZTE UFI103 (ZX_UFI103)
#
# Switches the dongle from AP mode to WiFi client mode, then starts the
# SMS gateway if it is not already running via the init service.
#
# USAGE (manual / development):
#   sudo adb shell "/system/xbin/librank /system/bin/sh /data/sms-gateway/scripts/wifi-client-start.sh"
#
# On a production boot the init service (sms-gw) calls wifi-setup.sh and
# start.sh automatically — this script is only needed for manual use.

GW_DIR=/data/sms-gateway

# ── Steps 1-7: WiFi client mode setup ────────────────────────────────────────
/system/bin/sh $GW_DIR/scripts/wifi-setup.sh

# ── Step 8: Start SMS gateway (if not already running via init service) ───────
if busybox ps 2>/dev/null | busybox grep -q '[s]ms-gateway'; then
    echo "sms-gateway is already running — not starting a second copy."
else
    echo "Starting sms-gateway..."
    $GW_DIR/sms-gateway >> $GW_DIR/sms-gateway.log 2>&1 &
    sleep 2
    echo "Gateway PID: $!"
fi

WLAN_IP=$(busybox ifconfig wlan0 2>/dev/null | busybox grep 'inet addr' | busybox sed 's/.*inet addr:\([^ ]*\).*/\1/')
echo "Web UI: http://${WLAN_IP:-192.168.100.1}/"
