#!/bin/bash
# Persistent su installation for ZTE UFI103 — Two-Phase Version
#
# Phase 1 (runs in background, survives SIGHUP):
#   - Remount /system rw
#   - Copy rootshell to /system/xbin/su
#   - Touch a marker file
#   - Does NOT chmod (that's the slow operation that races with USB disconnect)
#
# Phase 2 (runs after USB reconnect, fresh adb session):
#   - chmod 6755 /system/xbin/su
#   - chown 0:0 /system/xbin/su
#   - Remount /system ro
#   - Verify result
#
# Prerequisites (run ONCE per boot, files persist across reboots):
#   sudo adb push /tmp/cow           /data/local/tmp/cow
#   sudo adb push /tmp/rootshell3    /data/local/tmp/rootshell
#   sudo adb shell chmod 755 /data/local/tmp/cow /data/local/tmp/rootshell

set -e
ADB="sudo adb"
DEVICE_TMP="/data/local/tmp"

echo "=========================================="
echo "  Persistent su Installer v2 for ZTE UFI103"
echo "  Two-phase: copy before disconnect, chmod after reconnect"
echo "=========================================="
echo ""

# ── Check ADB ──
if ! $ADB devices 2>/dev/null | grep -q "device$"; then
    echo "ERROR: No ADB device. Make sure device is in Android mode (05c6:90b4)."
    exit 1
fi

# ── Check prerequisites ──
for f in cow rootshell; do
    if ! $ADB shell "ls $DEVICE_TMP/$f 2>/dev/null" | grep -q "$f"; then
        echo "ERROR: $DEVICE_TMP/$f not found on device."
        echo "Run first:"
        echo "  sudo adb push /tmp/cow        $DEVICE_TMP/cow"
        echo "  sudo adb push /tmp/rootshell3 $DEVICE_TMP/rootshell"
        echo "  sudo adb shell chmod 755 $DEVICE_TMP/cow $DEVICE_TMP/rootshell"
        exit 1
    fi
done

# ── Phase 0: Re-run dirty cow ──
echo "[Phase 0] Running dirty cow exploit..."
$ADB shell "$DEVICE_TMP/cow" 2>&1 | tail -3
echo ""
sleep 2

echo "[Phase 0] Verifying root access..."
ROOT_CHECK=$($ADB shell "/system/xbin/librank /system/bin/id" 2>/dev/null | tr -d '\r\n')
echo "    $ROOT_CHECK"
if ! echo "$ROOT_CHECK" | grep -q "uid=0"; then
    echo "ERROR: librank is not patched to rootshell. Retry dirty cow."
    exit 1
fi

# ── Phase 1: Background script (copy only, survives USB disconnect) ──
echo ""
echo "[Phase 1] Pushing inner script to device..."

# Create the Phase 1 device-side script
PHASE1_SCRIPT=$(mktemp /tmp/su_inner_v2.XXXXXX)
cat > "$PHASE1_SCRIPT" << 'INNER_EOF'
#!/system/bin/sh
# Phase 1: Runs on device. Ignores SIGHUP/TERM/INT.
# Only copies the rootshell to /system/xbin/su. Does NOT chmod.
trap '' HUP TERM INT
LOGFILE=/data/local/tmp/su_install_v2.log

echo "[$(date)] Phase 1 starting" > "$LOGFILE"

# Give adb shell time to exit cleanly before we trigger the USB disconnect
sleep 2

echo "[$(date)] Remounting /system rw..." >> "$LOGFILE"
MOUNT_OUT=$(mount -o remount,rw /system 2>&1)
echo "[$(date)]   mount output: $MOUNT_OUT" >> "$LOGFILE"

echo "[$(date)] Copying rootshell to su..." >> "$LOGFILE"
CP_OUT=$(cp /data/local/tmp/rootshell /system/xbin/su 2>&1)
echo "[$(date)]   cp output: $CP_OUT" >> "$LOGFILE"
CP_RC=$?
echo "[$(date)]   cp exit code: $CP_RC" >> "$LOGFILE"

if [ $CP_RC -eq 0 ]; then
    echo "[$(date)] Copy successful. Touching marker..." >> "$LOGFILE"
    touch /data/local/tmp/su_copy_done
    ls -la /system/xbin/su >> "$LOGFILE" 2>&1
    echo "[$(date)] Phase 1 complete (chmod NOT done yet)" >> "$LOGFILE"
else
    echo "[$(date)] COPY FAILED! Exit code $CP_RC" >> "$LOGFILE"
    ls -la /system/xbin/su >> "$LOGFILE" 2>&1
fi
INNER_EOF

$ADB push "$PHASE1_SCRIPT" "$DEVICE_TMP/su_inner_v2.sh"
rm -f "$PHASE1_SCRIPT"
$ADB shell "/system/xbin/librank /system/bin/chmod 755 $DEVICE_TMP/su_inner_v2.sh"

echo ""
echo "[Phase 1] Launching su_inner_v2.sh in background..."
echo "    Device will disconnect in ~2-3 seconds when /system is remounted rw."
echo ""

$ADB shell "/system/xbin/librank /system/bin/sh -c '$DEVICE_TMP/su_inner_v2.sh &'" 2>/dev/null || true

# ── Wait for USB disconnect and reconnect ──
echo "[Phase 1] Waiting for USB disconnect + reconnect..."
for i in $(seq 30 -1 1); do
    printf "\r    %2ds remaining..." "$i"
    sleep 1
done
echo ""

# Wait until device reappears
echo "[Phase 1] Waiting for device to reconnect..."
MAX_WAIT=60
for i in $(seq 1 $MAX_WAIT); do
    if $ADB devices 2>/dev/null | grep -q "device$"; then
        echo "    Device reconnected after $i seconds!"
        break
    fi
    if [ "$i" -eq "$MAX_WAIT" ]; then
        echo "ERROR: Device did not reconnect in ${MAX_WAIT}s."
        echo "Check USB connection. Phase 2 will not run automatically."
        echo "When device reconnects, run: sudo bash $0 --phase2"
        exit 1
    fi
    sleep 1
done

# Small delay to let adb settle
sleep 2

# ── Phase 2: chmod + chown (now that USB is stable) ──
echo ""
echo "[Phase 2] Setting SUID bit and ownership..."

# Verify the copy exists
COPY_CHECK=$($ADB shell "/system/xbin/librank /system/bin/ls -la /system/xbin/su 2>/dev/null || echo 'NOT_FOUND'")
echo "    Current state: $COPY_CHECK"

if echo "$COPY_CHECK" | grep -q "NOT_FOUND"; then
    echo "ERROR: /system/xbin/su was not copied. Phase 1 failed."
    echo "Check log:"
    $ADB shell "cat $DEVICE_TMP/su_install_v2.log 2>/dev/null"
    exit 1
fi

# Now chmod and chown — USB is stable, no race condition
echo "    Setting chmod 6755..."
$ADB shell "/system/xbin/librank /system/bin/chmod 6755 /system/xbin/su" 2>/dev/null || true
echo "    Setting chown 0:0..."
$ADB shell "/system/xbin/librank /system/bin/chown 0:0 /system/xbin/su" 2>/dev/null || true

# Remount ro
echo "    Remounting /system ro..."
$ADB shell "/system/xbin/librank /system/bin/mount -o remount,ro /system" 2>/dev/null || true

# Log the phase 2 actions
$ADB shell "/system/xbin/librank /system/bin/sh -c '
echo \"[$(date)] Phase 2: chmod/chown/remount-ro\" >> /data/local/tmp/su_install_v2.log
ls -la /system/xbin/su >> /data/local/tmp/su_install_v2.log 2>&1
'"

echo ""
echo "[Phase 2] Verifying..."
SU_STATE=$($ADB shell "ls -la /system/xbin/su 2>/dev/null || echo 'not found'")
echo "    $SU_STATE"

echo ""
echo "=========================================="
if echo "$SU_STATE" | grep -q "rws"; then
    echo "  SUCCESS: su is installed with SUID bit!"
    echo "=========================================="
    echo ""
    echo "Test with:"
    echo "  sudo adb shell /system/xbin/su -c id"
    echo "  sudo adb shell /system/xbin/su -c '/system/bin/id'"
    echo ""
    echo "Full install log:"
    $ADB shell "cat $DEVICE_TMP/su_install_v2.log 2>/dev/null"
else
    echo "  FAILED: SUID bit not set."
    echo "=========================================="
    echo ""
    echo "State: $SU_STATE"
    echo "Log:"
    $ADB shell "cat $DEVICE_TMP/su_install_v2.log 2>/dev/null"
    echo ""
    echo "The copy may have succeeded but chmod failed."
    echo "Check the log above for the specific error."
fi
