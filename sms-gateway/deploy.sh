#!/usr/bin/env bash
# deploy.sh — Build and push the SMS gateway to the ZTE UFI103 dongle.
#
# Usage:
#   ./deploy.sh            # build + push binary + scripts + boot hook
#   ./deploy.sh --no-build # skip build (push existing binary)
#   ./deploy.sh --no-boot  # skip boot hook setup
#
# Prerequisites:
#   - adb is in PATH; dongle is connected; root is available via librank.
#   - config.json must already exist at /data/sms-gateway/config.json on device,
#     or edit the PUSH_CONFIG line below to push it from a local file.

set -euo pipefail

ROOT_SH="/system/xbin/librank /system/bin/sh -c"
ROOT="/system/xbin/librank"

GW_DIR=/data/sms-gateway

# ── 0. Run tests ─────────────────────────────────────────────────────────────
echo "==> Running tests..."
go test ./... -count=1
echo "    All tests passed."

BUILD=1
SETUP_BOOT=1

for arg in "$@"; do
    case "$arg" in
        --no-build) BUILD=0 ;;
        --no-boot)  SETUP_BOOT=0 ;;
    esac
done

# ── 1. Build ─────────────────────────────────────────────────────────────────
if [ "$BUILD" -eq 1 ]; then
    echo "==> Building ARM binary..."
    cd "$(dirname "$0")"
    GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
        go build -ldflags="-s -w" -o sms-gateway ./cmd/sms-gateway
    echo "    $(ls -lh sms-gateway | awk '{print $5, $9}')"
fi

cd "$(dirname "$0")"

# ── 2. Verify device is present ──────────────────────────────────────────────
echo "==> Checking ADB..."
if ! sudo adb get-state 2>/dev/null | grep -q device; then
    echo "ERROR: no ADB device found. Is the dongle plugged in?"
    exit 1
fi

# ── 3. Ensure gateway directory exists ───────────────────────────────────────
echo "==> Ensuring $GW_DIR exists on device..."
sudo adb shell "$ROOT /system/bin/busybox mkdir -p $GW_DIR"

# ── 4. Kill running gateway (if any) ─────────────────────────────────────────
echo "==> Stopping existing gateway..."
sudo adb shell "$ROOT /system/bin/busybox pkill -f sms-gateway" 2>/dev/null || true
sleep 2

# ── 5. Push binary ───────────────────────────────────────────────────────────
echo "==> Pushing sms-gateway binary..."
sudo adb push sms-gateway $GW_DIR/sms-gateway
sudo adb shell "$ROOT /system/bin/busybox chmod 755 $GW_DIR/sms-gateway"

# ── 6. Push startup scripts ──────────────────────────────────────────────────
echo "==> Pushing startup scripts..."
sudo adb push scripts/start.sh $GW_DIR/start.sh
sudo adb shell "$ROOT /system/bin/busybox chmod 755 $GW_DIR/start.sh"
sudo adb shell "$ROOT /system/bin/busybox mkdir -p $GW_DIR/scripts"
sudo adb push scripts/wifi-setup.sh $GW_DIR/scripts/wifi-setup.sh
sudo adb push scripts/udhcpc.sh $GW_DIR/scripts/udhcpc.sh
sudo adb push scripts/wifi-client-start.sh $GW_DIR/scripts/wifi-client-start.sh
sudo adb push scripts/wifi-client-start.sh $GW_DIR/wifi-client-start.sh
sudo adb shell "$ROOT /system/bin/busybox chmod 755 $GW_DIR/scripts/wifi-setup.sh $GW_DIR/scripts/udhcpc.sh $GW_DIR/scripts/wifi-client-start.sh $GW_DIR/wifi-client-start.sh"

# ── 7. Push config example (skip if config already exists) ───────────────────
# Uncomment and edit the line below to push a local config.json:
# sudo adb push config.json $GW_DIR/config.json && \
#   sudo adb shell "$ROOT /system/bin/busybox chmod 600 $GW_DIR/config.json"

# ── 8. Boot persistence (init.target.rc) ──────────────────────────────────────
if [ "$SETUP_BOOT" -eq 1 ]; then
    echo "==> Checking boot persistence (init.target.rc)..."
    if sudo adb shell "$ROOT /system/bin/busybox grep -q 'service sms-gw' /init.target.rc" 2>/dev/null; then
        echo "    sms-gw service already registered in /init.target.rc — skipping."
    else
        echo "    Adding sms-gw service to /init.target.rc..."
        # Pull, patch, push back
        sudo adb shell "$ROOT /system/bin/busybox cp /init.target.rc $GW_DIR/init.target.rc.bak"
        sudo adb pull /data/sms-gateway/init.target.rc.bak /tmp/init.target.rc.deploy 2>/dev/null || \
            sudo adb shell "$ROOT /system/bin/busybox cp /init.target.rc $GW_DIR/init.target.rc.bak" && \
            sudo adb pull $GW_DIR/init.target.rc.bak /tmp/init.target.rc.deploy
        python3 - << 'PYEOF'
import sys
with open('/tmp/init.target.rc.deploy', 'r') as f:
    content = f.read()
SERVICE = '\nservice sms-gw /system/bin/sh /data/sms-gateway/start.sh\n    class main\n    user root\n    group root\n    disabled\n'
TRIGGER = 'on property:sys.boot_completed=1'
START = '   start sms-gw'
if 'service sms-gw' not in content:
    content = content.replace(TRIGGER, SERVICE + '\n' + TRIGGER)
    # Ensure blank line before trigger
    content = content.replace('    disabled\non property:', '    disabled\n\non property:')
if START not in content:
    content = content.replace(TRIGGER + '\n   start qrngp', TRIGGER + '\n   start qrngp\n' + START)
with open('/tmp/init.target.rc.deploy.new', 'w') as f:
    f.write(content)
assert 'service sms-gw' in content and START in content, "patch failed"
print("    init.target.rc patched ok")
PYEOF
        sudo adb push /tmp/init.target.rc.deploy.new $GW_DIR/init.target.rc.new
        sudo adb shell "$ROOT /system/bin/busybox mount -o remount,rw /"
        sudo adb shell "$ROOT /system/bin/sh -c 'cat $GW_DIR/init.target.rc.new > /init.target.rc'"
        sudo adb shell "$ROOT /system/bin/busybox mount -o remount,ro /"
        echo "    /init.target.rc updated — reboot required to activate."
    fi
    # Ensure .boot_started flag exists (disables the legacy debuggerd wrapper approach)
    sudo adb shell "$ROOT /system/bin/busybox touch $GW_DIR/.boot_started" 2>/dev/null || true
fi

# ── 9. Restart the gateway via init service ───────────────────────────────────
echo "==> Restarting sms-gateway..."
sudo adb shell "$ROOT /system/bin/busybox pkill -f 'start.sh|sms-gateway'" 2>/dev/null || true
sleep 2
sudo adb shell "$ROOT /system/bin/busybox nohup /system/bin/sh $GW_DIR/start.sh > /dev/null 2>&1 &"

echo ""
echo "==> Done. Gateway running on http://192.168.100.1/"
echo "    View log: sudo adb shell \"$ROOT /system/bin/busybox tail -f $GW_DIR/sms-gateway.log\""
echo "    Status:   curl http://192.168.100.1/status"
