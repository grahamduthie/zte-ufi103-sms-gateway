# ZTE UFI103 — Device Reference

## Hardware

| Property | Value |
|----------|-------|
| Model | ZX_UFI103 |
| Chipset | Qualcomm MSM8916 |
| CPU | Quad-core ARM Cortex-A53 (ARMv7, 32-bit) |
| RAM | 512MB |
| Android | 4.4.4 (KitKat) |
| Kernel | Linux 3.10.28, PREEMPT |
| Build | 2024-03-26, test-keys |
| Serial | 19ce8266 |
| IMEI | 867041077886923 |
| WiFi | Qualcomm WCNSS PRONTO — **2.4GHz only** |

## USB Modes

| Mode | VID:PID | Description |
|------|---------|-------------|
| Android | `05c6:90b4` | Normal operation — RNDIS + ADB |
| DIAG | `05c6:9091` | Diagnostic mode — entered after `/system` remount rw |
| EDL/QDL | `05c6:9008` | Emergency download — unplug, hold button, plug in |
| Fastboot | `18d1:d00d` | Bootloader — `adb reboot bootloader` |

## Root Access

`/system/xbin/librank` has been replaced on-disk with a 904-byte SUID rootshell.
Every execution runs as uid=0(root). Survives reboots.

```bash
adb shell "/system/xbin/librank /system/bin/id"
# uid=0(root) gid=2000(shell)
```

**Critical for gateway management**: The gateway process (started by the init
service) runs as root (uid=0). The adb shell is uid=2000 and cannot directly
kill root-owned processes. Use `librank` to kill the gateway:

```bash
adb shell "/system/xbin/librank /system/bin/busybox kill \
  \$(busybox ps | busybox awk '/sms-gateway$/{print \$1}')"
```

## Access Methods

### ADB
```bash
adb devices                    # → 19ce8266    device
adb shell                      # unprivileged shell (uid=2000)
adb shell "/system/xbin/librank /system/bin/sh"  # root shell
```

### Fastboot
```bash
adb reboot bootloader
fastboot devices               # → 19ce8266    fastboot
```

### EDL (Emergency Download)
Start the EDL tool BEFORE entering EDL mode.
Entry: unplug → hold button → plug in → release after 3s.

## Partition Map

| Partition | Block | Size | Notes |
|-----------|-------|------|-------|
| modem | mmcblk0p1 | 67MB | FAT, Qualcomm firmware blobs |
| sbl1 | mmcblk0p2 | 525KB | Signed ELF, contains Sahara code (IS the firehose programmer) |
| aboot | mmcblk0p4 | 1MB | Android bootloader (LK) |
| boot | mmcblk0p22 | 17MB | Android boot image |
| system | mmcblk0p23 | ext4, ro | Android system — librank modified here |
| persist | mmcblk0p24 | ext4, rw | Persist partition |
| cache | mmcblk0p25 | ext4, rw | Cache |
| userdata | mmcblk0p27 | 1.7GB ext4, rw | User data — gateway lives here |
| recovery | mmcblk0p26 | 17MB | Stock recovery |

Full backups in `backup/` directory.

## WiFi

### Mode Switch (AP → Client)

The `pronto_wlan.ko` driver only supports one mode at a time. To switch:

**Critical notes:**
- `[hostapd]` is a **kernel thread** (PID in brackets) — it CANNOT be killed.
  The only way to switch modes is `rmmod`/`insmod` to reload the driver.
- `busybox udhcpc` gets a DHCP lease but does **not** configure the interface
  IP on this device. You MUST set the IP and route manually after DHCP.
  `scripts/udhcpc.sh` handles this as a DHCP event script.
- Always remove bridge members (`brctl delif`) **before** bringing down wlan0
  and reloading the driver.
- **NEVER do rmmod/insmod more than once in a session.** Multiple driver reload
  cycles put the pronto_wlan driver into an unrecoverable state (wlan0
  disappears entirely). A clean reboot is the only recovery. The WiFi watchdog
  in the gateway performs soft reconnects (wpa_supplicant restart only) and
  never touches the driver module.

```bash
# 1. Stop AP services (dnsmasq is killable; hostapd is a kernel thread)
kill $(ps | grep dnsmasq | grep -v grep | awk '{print $1}')

# 2. Remove bridge members FIRST, then bring down bridge
brctl delif bridge1 wlan0
brctl delif bridge1 rndis0
ifconfig bridge1 down

# 3. Reload driver — this kills the [hostapd] kernel thread
ifconfig wlan0 down
rmmod wlan
sleep 3
insmod /system/lib/modules/pronto/pronto_wlan.ko
sleep 5

# 4. Write wpa_supplicant config (SSID is CASE-SENSITIVE)
cat > /data/misc/wifi/wpa_supplicant.conf << 'EOF'
ctrl_interface=/data/misc/wifi/sockets
update_config=1
ap_scan=1
network={
    ssid="EXACT_SSID_CASE"
    psk="password"
    key_mgmt=WPA-PSK
}
EOF

# 5. Bring up wlan0, then start wpa_supplicant
ifconfig wlan0 up
sleep 2
rm -f /data/misc/wifi/sockets/wlan0
/system/bin/wpa_supplicant -i wlan0 -D nl80211 \
  -c /data/misc/wifi/wpa_supplicant.conf \
  -O /data/misc/wifi/sockets -B
sleep 10

# 6. Get DHCP lease and configure interface (udhcpc.sh does the ifconfig/route)
busybox udhcpc -i wlan0 -q -n -s /data/sms-gateway/scripts/udhcpc.sh -x hostname:dongle

# 7. Restore rndis0 for USB access
ifconfig rndis0 192.168.100.1 netmask 255.255.255.0 up
```

### Key WiFi Facts
- `wpa_supplicant` SSID matching is **case-sensitive**
- `hostapd` is a kernel thread — cannot be killed, respawns via init
- After mode switch, the SIM may re-lock (PIN: 8837, but permanently disabled)
- DNS is not available on the device — must use IP addresses for external services
- The WiFi watchdog goroutine in the gateway does soft reconnects every 45s
  if wlan0 loses its IP (kills and restarts wpa_supplicant, runs udhcpc again)

## Network Configuration

| Property | Value |
|----------|-------|
| WiFi SSID | YOUR_WIFI_SSID_1 |
| WiFi IP (dynamic) | 172.16.10.x (DHCP from router) |
| USB (rndis0) IP | 192.168.100.1 |
| Host USB access | `sudo ip addr add 192.168.100.2/24 dev enx02030f556538` |
| Web UI | http://192.168.100.1:8080/ (over rndis0 or WiFi) |

## Running Processes (Key)

| User | Process | Notes |
|------|---------|-------|
| radio | `/system/bin/rild` | RIL daemon — manages modem via QMI; see RILD section |
| system | `com.wowi.nanowebyl` | Stock web UI server (port 8000) |
| nobody | `/system/bin/dnsmasq` | DNS/DHCP for hotspot (AP mode) |
| wifi | `[hostapd]` | WiFi AP kernel thread — cannot be killed |
| shell | `/sbin/adbd` | ADB daemon |
| root | `/system/bin/sh .../start.sh` | Gateway wrapper (init service) |
| root | `/data/sms-gateway/sms-gateway` | Gateway binary |

## RILD and SMS Architecture

### SMD Channels (Qualcomm Shared Memory Driver)

The MSM8916 exposes multiple independent SMD channels between the application
processor and the modem processor:

| Device | Protocol | Primary User | Notes |
|--------|----------|-------------|-------|
| `/dev/smd11` | AT commands | RILD + gateway | Shared; both processes keep fds open |
| `/dev/smd36` | QMI WMS | RILD | Primary SMS channel (Qualcomm binary protocol) |
| `/dev/smd7` | QMI CTL | RILD | QMI control |
| `/dev/smd8` | QMI NAS | RILD | Network/signal |

### How RILD handles SMS on this device

*See `SMS_MODEM_ARCHITECTURE.md` for full research notes.*

RILD uses **QMI WMS** (over `/dev/smd36`) — NOT AT commands — as its primary
SMS receive path. RILD sets `AT+CNMI=0,0,0,0,0` at boot; with `mt=0` the modem
routes all incoming SMS via QMI only and does **not** write them to SIM (SM)
storage. Our gateway overrides this with `AT+CNMI=2,1,0,0,0` (`mt=1`) on every
poll, causing the modem to write incoming SMS to SM storage AND send `+CMTI`
notifications. Messages persist on SIM until our gateway reads them with
`AT+CMGL` and deletes them with `AT+CMGD`.

**Key facts for the gateway:**
- `AT+CNMI=2,1,0,0,0` must be maintained; without it, SM storage stays empty
- `+CMTI:` URCs DO fire on our fd when `mt=1` is active — `NewMessageCh` in
  `session.go` receives them and triggers an immediate poll
- RILD sets `AT+CNMI` only at boot (not periodically); there is a theoretical
  window of ~3–4s between our re-applications — see `SMS_MODEM_ARCHITECTURE.md`
- `com.qualcomm.telephony` replaces the standard Android telephony provider.
  The standard Android SMS database
  (`/data/data/com.android.providers.telephony/databases/mmssms.db`) is
  permanently empty on this device. The `pollAndroidSMS()` fallback is dead code.

### AT command sharing between RILD and gateway

RILD keeps `/dev/smd11` open with a blocking `read()` permanently in flight.
The gateway also keeps a persistent fd open with its own blocking `readerLoop`.
The kernel delivers modem responses to whichever `read()` call is scheduled
first. RILD also issues `AT+CPMS?` every 3-5 seconds, interleaving with our
commands.

This is why:
- The gateway uses a **persistent fd** (opened once at startup, never closed)
  with a continuous background reader — so our reader competes as a peer
- The gateway uses **position-based buffer slicing** to extract its own
  responses from the accumulated buffer, ignoring RILD noise
- Per-command open/read/close does NOT work (RILD steals responses)

## AT Commands

All AT commands go via `/dev/smd11` through the Go gateway's persistent reader.

| Command | Purpose | Response |
|---------|---------|----------|
| `AT+CPIN?` | Check SIM lock status | `+CPIN: READY` |
| `AT+CPMS="SM","SM","SM"` | Set SMS storage to SIM | `+CPMS: "SM",N,20,...` |
| `AT+CMGF=1` | Set text mode | `OK` |
| `AT+CMGL="ALL"` | List all messages | `+CMGL: index,"status","sender",,"timestamp"\r\nbody` |
| `AT+CMGD=index,0` | Delete message by index | `OK` |
| `AT+CMGS="+number"\r` | Start SMS send (wait for `> `) | `> ` prompt |
| `<text>\x1a` | Send text + Ctrl-Z | `+CMGS: <ref>` |
| `AT+CSQ` | Signal quality | `+CSQ: <rssi>,<ber>` |
| `AT+CREG?` | Network registration | `+CREG: 0,1` (registered) |
| `AT+COPS?` | Operator name | `+COPS: 0,0,"spusu spusu",7` |
| `AT+CNMI?` | New message indication config | `+CNMI: 0,0,0,0,0` (no URCs to TE) |

## BusyBox Version Notes

**BusyBox v1.23.0 (2014-08-22)** is installed on this device. Known limitations:
- `flock -n <fd>` (numeric file descriptor form) is **NOT supported**. Always use
  `flock lockfile cmd` form or the PID file approach. See BUGS.md for how this
  caused a critical failure.
- `timeout` is present but some flags differ from modern versions
- `stat -c%s` works for file size

## EDL / QDL Mode

The SBL1 (`backup/sbl1.img`) IS the signed firehose programmer for this device.
Generic `prog_emmc_firehose_8916.mbn` files are rejected by secure boot.

```bash
# Start BEFORE entering EDL mode
sudo python3 /tmp/edl/edl.py \
  --vid=0x05c6 --pid=0x9008 \
  --loader=/home/marlowfm/dongle/backup/sbl1.img \
  printgpt --memory=emmc
```

---

*See also: `STATUS.md` (quick start), `GATEWAY.md` (SMS gateway architecture), `BUGS.md` (known issues and root cause analysis), `SMS_MODEM_ARCHITECTURE.md` (CNMI/QMI SMS routing research)*
