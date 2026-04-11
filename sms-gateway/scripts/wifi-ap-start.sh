#!/system/bin/sh
# wifi-ap-start.sh — switch wlan0 to AP (hostapd) mode for WiFi setup portal.
#
# Called by start.sh when WiFi client mode fails (no known network in range,
# wrong credentials, or force_ap_mode set in config.json).
#
# After this script exits, start.sh launches sms-gateway --setup-mode, which
# serves the captive portal setup page at http://192.168.100.1/
#
# Run via: /system/xbin/librank /system/bin/sh /data/sms-gateway/scripts/wifi-ap-start.sh

GW_DIR=/data/sms-gateway

echo "[$(date)] wifi-ap-start.sh: starting AP mode"

# ── Step 1: Tear down client mode ────────────────────────────────────────────
busybox killall wpa_supplicant 2>/dev/null || true
busybox sleep 2

# ── Step 2: Driver reload (CLIENT → AP) ──────────────────────────────────────
# AP mode (hostapd) requires rmmod/insmod to switch from client mode.
# This is the one driver reload per boot — within the pronto safety margin.
busybox ifconfig wlan0 down 2>/dev/null || true
rmmod wlan 2>/dev/null || true
busybox sleep 3
insmod /system/lib/modules/pronto/pronto_wlan.ko
busybox sleep 5

# ── Step 3: Bring up wlan0 with static IP ────────────────────────────────────
busybox ifconfig wlan0 192.168.100.1 netmask 255.255.255.0 up

# ── Step 4: Start hostapd (SMS-Gateway-Setup hotspot) ────────────────────────
busybox killall hostapd 2>/dev/null || true
busybox sleep 1
/system/bin/hostapd -e /data/misc/wifi/entropy.bin \
    $GW_DIR/scripts/hostapd-setup.conf -B
busybox sleep 2

# ── Step 5: dnsmasq — DHCP + wildcard DNS redirect (captive portal) ──────────
busybox killall dnsmasq 2>/dev/null || true
busybox sleep 1
dnsmasq \
    --interface=wlan0 \
    --dhcp-range=192.168.100.2,192.168.100.254,1h \
    --address=/#/192.168.100.1 \
    --no-resolv \
    --no-hosts \
    --pid-file=$GW_DIR/dnsmasq.pid \
    --log-facility=/dev/null

# ── Step 6: iptables — captive portal redirect ────────────────────────────────
# Redirect all port 80 and 443 from wlan0 clients to our setup server.
iptables -t nat -F PREROUTING 2>/dev/null || true
iptables -t nat -A PREROUTING -i wlan0 -p tcp --dport 80 \
    -j DNAT --to-destination 192.168.100.1:80
iptables -t nat -A PREROUTING -i wlan0 -p tcp --dport 443 \
    -j DNAT --to-destination 192.168.100.1:80

# ── Step 7: Restore rndis0 for USB/ADB access ────────────────────────────────
busybox ifconfig rndis0 192.168.100.1 netmask 255.255.255.0 up

echo "[$(date)] wifi-ap-start.sh: AP mode ready — SMS-Gateway-Setup hotspot at 192.168.100.1"
