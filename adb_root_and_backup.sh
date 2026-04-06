#!/bin/bash
# ADB root + partition backup for ZTE UFI103 (userdebug build)
# 
# On userdebug builds, 'adb root' restarts adbd with root privileges.
# This allows reading raw block devices, bypassing the EDL loader issue.

set -e
BACKUP_DIR="./backup_$(date +%Y%m%d_%H%M%S)"

echo "=== ZTE UFI103 ADB Root + Backup ==="
echo ""

# Check device
if ! sudo adb devices | grep -q "device$"; then
    echo "ERROR: No ADB device found. Make sure device is in Android mode."
    echo "  - If in fastboot: sudo fastboot reboot"
    echo "  - If in EDL: unplug, hold button for 10s to force normal boot"
    exit 1
fi

echo "Device detected. Trying 'adb root'..."
RESULT=$(sudo adb root 2>&1)
echo "  Result: $RESULT"

sleep 2

if sudo adb shell id | grep -q "uid=0"; then
    echo "ROOT ACQUIRED! uid=0 confirmed."
elif echo "$RESULT" | grep -q "already running as root\|restarting adbd as root"; then
    echo "Root mode confirmed."
else
    echo "WARNING: adb root may have failed. Checking current uid..."
    sudo adb shell id
fi

echo ""
echo "=== Device Info ==="
sudo adb shell getprop ro.build.fingerprint
sudo adb shell cat /proc/version
sudo adb shell ls /dev/block/bootdevice/by-name/ 2>/dev/null || \
  sudo adb shell ls /dev/block/platform/*/by-name/ 2>/dev/null || \
  echo "Could not list partition names"

echo ""
mkdir -p "$BACKUP_DIR"
echo "=== Backing up partitions to $BACKUP_DIR ==="

# List partition mappings
sudo adb shell ls -la /dev/block/bootdevice/by-name/ 2>/dev/null > "$BACKUP_DIR/partition_map.txt" || \
  sudo adb shell ls -la /dev/block/platform/*/by-name/ 2>/dev/null > "$BACKUP_DIR/partition_map.txt"

echo "Partition map saved."

# Critical partitions to backup
PARTITIONS="boot recovery system persist modem sbl1 rpm aboot tz"

for PART in $PARTITIONS; do
    echo -n "  Pulling $PART... "
    # Try by-name first, fall back to direct mmcblk
    BLOCK=$(sudo adb shell "readlink -f /dev/block/bootdevice/by-name/$PART 2>/dev/null || readlink -f /dev/block/platform/*/by-name/$PART 2>/dev/null" 2>/dev/null | tr -d '\r')
    if [ -n "$BLOCK" ]; then
        sudo adb shell "dd if=$BLOCK 2>/dev/null" > "$BACKUP_DIR/${PART}.img" 2>/dev/null
        SIZE=$(wc -c < "$BACKUP_DIR/${PART}.img")
        echo "OK ($SIZE bytes)"
    else
        echo "SKIP (partition not found)"
    fi
done

# Also try to get the sbl1 (contains or references the firehose loader)
echo ""
echo "=== Extracting SBL1 (contains firehose loader reference) ==="
for BLOCK in /dev/block/mmcblk0p1 /dev/block/mmcblk0p2 /dev/block/mmcblk0p3; do
    echo -n "  Trying $BLOCK... "
    sudo adb shell "dd if=$BLOCK count=1024 2>/dev/null" > "$BACKUP_DIR/$(basename $BLOCK).img" 2>/dev/null
    SIZE=$(wc -c < "$BACKUP_DIR/$(basename $BLOCK).img")
    MAGIC=$(xxd -l 4 "$BACKUP_DIR/$(basename $BLOCK).img" 2>/dev/null)
    echo "size=$SIZE magic=$MAGIC"
done

echo ""
echo "=== Check for firehose/loader in pulled images ==="
for F in "$BACKUP_DIR"/*.img; do
    if file "$F" | grep -q "ELF"; then
        echo "  ELF found: $F"
        file "$F"
    fi
done

echo ""
echo "Backup complete in $BACKUP_DIR"
echo ""
echo "=== Next: Flash via ADB sideload or EDL with correct loader ==="
echo "  - The 'sbl1.img' or 'aboot.img' may contain/reference the firehose loader"
echo "  - Use 'binwalk' or 'strings' to search for embedded ELF files in these images"
