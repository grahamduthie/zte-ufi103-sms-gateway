#!/bin/bash
# Make root permanent on ZTE UFI103 by replacing /system/xbin/librank with rootshell.
#
# This is a ONE-TIME setup script. After running it, root is available
# permanently via "adb shell /system/xbin/librank" without needing dirty cow.
#
# Prerequisites:
#   - Device connected in Android mode (05c6:90b4)
#   - /data/local/tmp/cow and /data/local/tmp/rootshell present on device
#   - Dirty cow exploit working (CVE-2016-5195)

set -e
ADB="sudo adb"
DEVICE_TMP="/data/local/tmp"

echo "=========================================="
echo "  Permanent Root Installer for ZTE UFI103"
echo "  Replaces /system/xbin/librank with rootshell"
echo "=========================================="
echo ""

# Check ADB
if ! $ADB devices 2>/dev/null | grep -q "device$"; then
    echo "ERROR: No ADB device. Make sure device is in Android mode (05c6:90b4)."
    exit 1
fi

# Check prerequisites
for f in cow rootshell; do
    if ! $ADB shell "ls $DEVICE_TMP/$f 2>/dev/null" | grep -q "$f"; then
        echo "ERROR: $DEVICE_TMP/$f not found on device."
        echo "Push first:"
        echo "  sudo adb push /tmp/cow        $DEVICE_TMP/cow"
        echo "  sudo adb push /tmp/rootshell3 $DEVICE_TMP/rootshell"
        echo "  sudo adb shell chmod 755 $DEVICE_TMP/cow $DEVICE_TMP/rootshell"
        exit 1
    fi
done

# Step 1: Run dirty cow
echo "[1] Running dirty cow exploit..."
$ADB shell "$DEVICE_TMP/cow" 2>&1 | tail -3
sleep 2

# Verify root
echo "[2] Verifying root..."
ROOT_CHECK=$($ADB shell "/system/xbin/librank /system/bin/id" 2>/dev/null | tr -d '\r\n')
echo "    $ROOT_CHECK"
if ! echo "$ROOT_CHECK" | grep -q "uid=0"; then
    echo "ERROR: Root not available. Dirty cow may have failed."
    exit 1
fi

# Step 2: Replace librank on-disk
echo ""
echo "[3] Replacing /system/xbin/librank with rootshell..."
echo "    This will trigger a USB mode switch to DIAG for ~45 seconds."
echo "    The write WILL persist — just wait for the device to return."
echo ""

$ADB shell "/system/xbin/librank /system/bin/busybox sh -c '
mount -o remount,rw /system
cp /data/local/tmp/rootshell /system/xbin/librank
sync
mount -o remount,ro /system
'" 2>/dev/null || true

echo "[4] Waiting for USB mode cycle (~45 seconds)..."
for i in $(seq 60 -1 1); do
    if [ $((i % 5)) -eq 0 ]; then
        printf "\r    %2ds remaining..." "$i"
    fi
    sleep 1
done
echo ""

# Wait until device returns to Android mode
echo "[5] Waiting for device to return to Android mode..."
MAX_WAIT=120
for i in $(seq 1 $MAX_WAIT); do
    MODE=$(lsusb 2>/dev/null | grep "05c6" | grep -o "90b4\|9091\|9008" | head -1)
    if [ "$MODE" = "90b4" ]; then
        echo "    Device back in Android mode after ${i}s!"
        break
    fi
    if [ "$MODE" = "9091" ]; then
        printf "\r    Still in DIAG mode... (%ds)" "$i"
    fi
    if [ "$i" -eq "$MAX_WAIT" ]; then
        echo ""
        echo "WARNING: Device did not return to Android mode in ${MAX_WAIT}s."
        echo "Check: lsusb | grep 05c6"
        echo "If device is in DIAG mode (9091), just wait longer — it will return."
    fi
    sleep 1
done

sleep 2

# Step 3: Verify
echo ""
echo "[6] Verifying permanent root..."
if ! $ADB devices 2>/dev/null | grep -q "device$"; then
    echo "ERROR: Device not connected."
    exit 1
fi

SU_STATE=$($ADB shell "ls -la /system/xbin/librank" 2>/dev/null)
echo "    $SU_STATE"

# Test root access
ROOT_TEST=$($ADB shell "/system/xbin/librank /system/bin/id" 2>/dev/null | tr -d '\r\n')
echo "    Root test: $ROOT_TEST"

echo ""
echo "=========================================="
if echo "$ROOT_TEST" | grep -q "uid=0"; then
    echo "  SUCCESS: Permanent root installed!"
    echo "=========================================="
    echo ""
    echo "From now on, root is available without dirty cow:"
    echo "  sudo adb shell '/system/xbin/librank /system/bin/id'"
    echo "  sudo adb shell '/system/xbin/librank /system/bin/sh'"
else
    echo "  FAILED: Root test did not return uid=0."
    echo "=========================================="
    echo ""
    echo "The librank file may not have been written correctly."
    echo "Check: sudo adb shell 'ls -la /system/xbin/librank'"
    echo "If needed, re-run this script."
fi
