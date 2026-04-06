#!/system/bin/sh
# udhcpc event script — configures wlan0 when a DHCP lease is obtained.
#
# Called by: busybox udhcpc -s /data/sms-gateway/scripts/udhcpc.sh
# busybox sets these environment variables before calling us:
#   $interface  — interface name (wlan0)
#   $ip         — assigned IP address
#   $subnet     — subnet mask
#   $router     — default gateway IP
#   $dns        — space-separated list of DNS servers

case "$1" in
    bound|renew)
        ifconfig "$interface" "$ip" netmask "${subnet:-255.255.255.0}" up
        if [ -n "$router" ]; then
            ip route del default 2>/dev/null
            ip route add default via "$router" dev "$interface"
        fi
        if [ -n "$dns" ]; then
            setprop net.dns1 "${dns%% *}"
        fi
        echo "[udhcpc] bound $interface to $ip via $router"
        ;;
    deconfig)
        ifconfig "$interface" 0.0.0.0 2>/dev/null
        ;;
esac
