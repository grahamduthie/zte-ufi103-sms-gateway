#!/system/bin/sh
# wifi-setup.sh — switches the dongle from AP mode to WiFi client mode.
#
# Handles steps 1-9 of the mode switch: driver reload, wpa_supplicant,
# DHCP (dynamic IP via udhcpc.sh). Does NOT start the SMS gateway.
#
# Called by: start.sh (on boot) and wifi-client-start.sh (manual).
# Run via: /system/xbin/librank /system/bin/sh /data/sms-gateway/scripts/wifi-setup.sh
#
# WiFi credentials — edit here or deploy a new script via adb push.
# These will eventually be read from config.json by the WiFi AP manager.
# Multiple networks are supported — wpa_supplicant will try them in priority order.

# Primary network
WIFI_SSID="YOUR_WIFI_SSID_1"
WIFI_PSK="YOUR_WIFI_PASSWORD"

# Secondary network
WIFI_SSID_2="YOUR_WIFI_SSID_2"
WIFI_PSK_2="YOUR_WIFI_PASSWORD"

GW_DIR=/data/sms-gateway

echo "[wifi-setup] Switching to client mode, SSID: $WIFI_SSID"

# ── Step 1: Kill AP-mode services ─────────────────────────────────────────────
# dnsmasq can be killed; hostapd is a kernel thread (cannot be killed directly)
busybox kill $(busybox ps | busybox grep dnsmasq | busybox grep -v grep | busybox awk '{print $1}') 2>/dev/null
sleep 1

# ── Step 2: Remove bridge members and bring down bridge ───────────────────────
brctl delif bridge1 wlan0 2>/dev/null
brctl delif bridge1 rndis0 2>/dev/null
ifconfig bridge1 down 2>/dev/null

# ── Step 3: Reload WiFi driver ────────────────────────────────────────────────
# The only way to switch AP→client: rmmod/insmod kills the [hostapd] kernel thread
ifconfig wlan0 down 2>/dev/null
rmmod wlan
sleep 3
insmod /system/lib/modules/pronto/pronto_wlan.ko
sleep 5

# ── Step 4: Write wpa_supplicant config ───────────────────────────────────────
busybox cat > /data/misc/wifi/wpa_supplicant.conf << EOF
ctrl_interface=/data/misc/wifi/sockets
update_config=1
ap_scan=1
network={
    ssid="${WIFI_SSID}"
    psk="${WIFI_PSK}"
    key_mgmt=WPA-PSK
    priority=1
}
network={
    ssid="${WIFI_SSID_2}"
    psk="${WIFI_PSK_2}"
    key_mgmt=WPA-PSK
    priority=2
}
EOF

# ── Step 5: Start wpa_supplicant ─────────────────────────────────────────────
ifconfig wlan0 up
sleep 2
busybox rm -f /data/misc/wifi/sockets/wlan0
/system/bin/wpa_supplicant -i wlan0 -D nl80211 \
    -c /data/misc/wifi/wpa_supplicant.conf \
    -O /data/misc/wifi/sockets -B
sleep 10

# ── Step 6: DHCP with dynamic IP ─────────────────────────────────────────────
# udhcpc.sh sets the IP, route, and DNS when the lease is bound.
busybox udhcpc -i wlan0 -q -n \
    -s $GW_DIR/scripts/udhcpc.sh \
    -x hostname:dongle
DHCP_RC=$?
sleep 1

if [ $DHCP_RC -ne 0 ]; then
    echo "[wifi-setup] DHCP failed (rc=$DHCP_RC) — no IP assigned. IMAP/SMTP will not work."
else
    WLAN_IP=$(busybox ifconfig wlan0 2>/dev/null | busybox grep 'inet addr' | busybox sed 's/.*inet addr:\([^ ]*\).*/\1/')
    echo "[wifi-setup] DHCP ok — wlan0: $WLAN_IP"
fi

# ── Step 7: Restore rndis0 for USB/ADB access ────────────────────────────────
ifconfig rndis0 192.168.100.1 netmask 255.255.255.0 up

echo "[wifi-setup] Done."
