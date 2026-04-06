#!/usr/bin/env python3
"""
Fast-catch EDL helper for ZTE UFI103 MSM8916
Fixes the timing issue where the HELLO_REQ is missed.

Usage:
  1. Run this script FIRST (it will wait for device)
  2. Enter EDL mode: unplug device, hold reset button, plug in
  3. Script catches HELLO_REQ and enters Sahara command mode
     to read HWID and PK_HASH, then tries to upload a loader.

Loader search order:
  1. Exact match: {hwid}_{pkhash16}.mbn in LOADERS_DIR
  2. HWID match (any pkhash)
  3. Fallback list in FALLBACK_LOADERS

Based on diagnosis: the bkerler edl tool uses timeout=1ms on initial
USB read, missing the HELLO_REQ. This script uses a proper timeout.
"""
import usb.core
import usb.util
import struct
import sys
import os
import time
import glob

VENDOR_ID  = 0x05c6
PRODUCT_ID = 0x9008

# Sahara commands
SAHARA_HELLO_REQ       = 0x01
SAHARA_HELLO_RSP       = 0x02
SAHARA_READ_DATA       = 0x03
SAHARA_END_TRANSFER    = 0x04
SAHARA_DONE_REQ        = 0x05
SAHARA_DONE_RSP        = 0x06
SAHARA_RESET_REQ       = 0x07
SAHARA_RESET_RSP       = 0x08
SAHARA_MEMORY_DEBUG    = 0x09
SAHARA_MEMORY_READ     = 0x0A
SAHARA_CMD_READY       = 0x0B
SAHARA_SWITCH_MODE     = 0x0C
SAHARA_EXECUTE_REQ     = 0x0D
SAHARA_EXECUTE_RSP     = 0x0E
SAHARA_EXECUTE_DATA    = 0x0F

# Sahara execute commands
EXEC_SERIAL_NUM        = 0x01
EXEC_MSM_HW_ID         = 0x02
EXEC_OEM_PK_HASH       = 0x03

# Sahara modes
MODE_IMAGE_TX_PENDING  = 0x0
MODE_IMAGE_TX_COMPLETE = 0x1
MODE_MEMORY_DEBUG      = 0x2
MODE_COMMAND           = 0x3

# Loader directories to search
LOADERS_DIR = "/tmp"
FALLBACK_LOADERS = [
    "/tmp/prog_emmc_firehose_8916.mbn",
    "/tmp/8916.mbn",
    "/tmp/0000000000000000_2ec22142a340cdad_fhprg_peek.bin",
    "/tmp/0000000000000000_a74b29c7842a5c51_fhprg_peek.bin",
    "/tmp/0000000000000000_bd65d9475cbd3924_fhprg_peek.bin",
]

ep_in  = None
ep_out = None

def usb_read(timeout_ms=3000):
    """Read from USB bulk endpoint with proper timeout."""
    try:
        data = bytes(ep_in.read(512, timeout_ms))
        if data:
            if len(data) >= 8:
                cmd, length = struct.unpack_from('<II', data)
                print(f"  [RX] cmd={hex(cmd)} len={hex(length)}: {data.hex()}")
            else:
                print(f"  [RX] short: {data.hex()}")
        return data
    except usb.core.USBTimeoutError:
        return b""
    except Exception as e:
        print(f"  [RX] Error: {e}")
        return None

def usb_write(data):
    """Write to USB bulk endpoint."""
    print(f"  [TX] {data.hex()}")
    ep_out.write(data)

def send_hello_rsp(mode, version=2, version_min=1):
    """Send SAHARA_HELLO_RSP."""
    pkt = struct.pack("<IIIIIIIIIIII",
        SAHARA_HELLO_RSP, 0x30, version, version_min, 0, mode,
        1, 2, 3, 4, 5, 6)
    usb_write(pkt)

def send_execute_req(cmd_id):
    usb_write(struct.pack("<III", SAHARA_EXECUTE_REQ, 12, cmd_id))

def send_execute_data(cmd_id):
    usb_write(struct.pack("<III", SAHARA_EXECUTE_DATA, 12, cmd_id))

def cmd_exec(exec_cmd):
    """Execute a Sahara command and return the response data."""
    send_execute_req(exec_cmd)
    data = usb_read(2000)
    if not data or len(data) < 16:
        return None
    cmd = struct.unpack_from('<I', data)[0]
    if cmd != SAHARA_EXECUTE_RSP:
        print(f"  Expected EXECUTE_RSP (0x0E), got {hex(cmd)}")
        return None
    data_len = struct.unpack_from('<I', data, 12)[0]
    send_execute_data(exec_cmd)
    payload = usb_read(data_len + 1000)
    return bytes(payload) if payload else None

def find_loader(hwidstr, pkhash_prefix):
    """Search for a loader matching HWID and PK_HASH."""
    # Try exact match in loaders dir
    if hwidstr and pkhash_prefix:
        pattern = os.path.join(LOADERS_DIR, f"{hwidstr}_{pkhash_prefix}*.bin")
        matches = glob.glob(pattern)
        if matches:
            print(f"  Found exact loader: {matches[0]}")
            return matches[0]

        pattern = os.path.join(LOADERS_DIR, f"{hwidstr}_{pkhash_prefix}*.mbn")
        matches = glob.glob(pattern)
        if matches:
            return matches[0]

    # Try fallback loaders
    for loader in FALLBACK_LOADERS:
        if os.path.exists(loader):
            print(f"  Using fallback loader: {loader}")
            return loader

    return None

def upload_loader(loader_path):
    """Upload a firehose loader binary via Sahara."""
    print(f"\n=== Uploading loader: {loader_path} ===")
    with open(loader_path, 'rb') as f:
        loader_data = f.read()

    print(f"  Loader size: {len(loader_data)} bytes")
    send_hello_rsp(MODE_IMAGE_TX_PENDING)

    loop = 0
    while True:
        data = usb_read(5000)
        if not data:
            print("  No response from device")
            return False

        cmd = struct.unpack_from('<I', data)[0]

        if cmd == SAHARA_READ_DATA:
            image_id, data_offset, data_len = struct.unpack_from('<III', data, 8)
            print(f"  READ_DATA: image_id={hex(image_id)}, offset={hex(data_offset)}, len={hex(data_len)}")
            if loop == 0:
                if image_id >= 0xC:
                    print("  Firehose mode detected")
                elif image_id == 0x7:
                    print("  NAND mode detected")
                else:
                    print(f"  Unknown image mode: {hex(image_id)}")
            # Send the requested chunk
            chunk = loader_data[data_offset:data_offset + data_len]
            if len(chunk) < data_len:
                chunk += b'\xFF' * (data_len - len(chunk))
            usb_write(chunk)
            loop += 1

        elif cmd == SAHARA_END_TRANSFER:
            image_id, status = struct.unpack_from('<II', data, 8)
            if status == 0:
                print("  Loader transfer SUCCESS")
                # Send DONE_REQ
                usb_write(struct.pack("<II", SAHARA_DONE_REQ, 8))
                resp = usb_read(3000)
                if resp and len(resp) >= 4:
                    done_cmd = struct.unpack_from('<I', resp)[0]
                    if done_cmd == SAHARA_DONE_RSP:
                        print("  DONE_RSP received - loader is running!")
                        return True
                return True
            else:
                print(f"  Loader transfer FAILED, status={hex(status)}")
                print(f"  image_id={hex(image_id)}")
                return False

        elif cmd == SAHARA_DONE_REQ:
            print("  Device sent DONE_REQ directly")
            usb_write(struct.pack("<II", SAHARA_DONE_REQ, 8))
            return True

        else:
            print(f"  Unexpected response: cmd={hex(cmd)}")
            return False

    return False

def run_command_mode():
    """Enter Sahara command mode and read device info."""
    print("\n=== Entering Sahara command mode ===")
    send_hello_rsp(MODE_COMMAND)
    data = usb_read(2000)
    if not data:
        print("  No response to COMMAND mode hello")
        return None, None

    cmd = struct.unpack_from('<I', data)[0]
    if cmd != SAHARA_CMD_READY:
        print(f"  Expected CMD_READY (0x0B), got {hex(cmd)}")
        return None, None

    print("  CMD_READY received!")

    # Read serial number
    serial_data = cmd_exec(EXEC_SERIAL_NUM)
    serial = "unknown"
    if serial_data:
        serial = f"{int.from_bytes(serial_data[:4], 'little'):08x}"
        print(f"  Serial: {serial}")

    # Read MSM HWID
    hwid_data = cmd_exec(EXEC_MSM_HW_ID)
    hwidstr = None
    if hwid_data and len(hwid_data) >= 8:
        hwid = int.from_bytes(hwid_data[:8], 'little')
        hwidstr = f"{hwid:016x}"
        msm_id = int(hwidstr[2:8], 16)
        oem_id = int(hwidstr[8:12], 16)
        model_id = int(hwidstr[12:16], 16)
        print(f"  HWID: {hwidstr}")
        print(f"  MSM_ID: {hex(msm_id)}, OEM_ID: {hex(oem_id)}, MODEL_ID: {hex(model_id)}")

    # Read OEM PK Hash
    pkhash_data = cmd_exec(EXEC_OEM_PK_HASH)
    pkhash = None
    if pkhash_data:
        pkhash = pkhash_data.hex()
        pkhash_prefix = pkhash[:16]
        print(f"  PK_HASH: {pkhash}")
        print(f"  PK_HASH prefix (for loader filename): {pkhash_prefix}")

        # Check if device is unfused (generic hash)
        unfused_hashes = ["0000000000000000", "ffffffffffffffff"]
        if pkhash_prefix.lower() in unfused_hashes:
            print("  Device appears UNFUSED - any loader should work!")

    return hwidstr, pkhash

def main():
    global ep_in, ep_out

    print("=== ZTE UFI103 EDL Helper (Timing-Fixed) ===")
    print("Run this script BEFORE entering EDL mode.")
    print("Then: unplug device, hold reset button, plug in.\n")

    # Wait for device
    print("Waiting for 05c6:9008 (Qualcomm QDL)...")
    dev = None
    deadline = time.time() + 120  # wait up to 2 minutes
    while time.time() < deadline:
        dev = usb.core.find(idVendor=VENDOR_ID, idProduct=PRODUCT_ID)
        if dev:
            print(f"Device found! Bus {dev.bus} Device {dev.address}")
            break
        time.sleep(0.05)  # poll every 50ms

    if not dev:
        print("Timeout waiting for device.")
        sys.exit(1)

    # Setup USB
    try:
        if dev.is_kernel_driver_active(0):
            dev.detach_kernel_driver(0)
            print("Detached kernel driver")
    except Exception as e:
        print(f"Detach: {e}")

    try:
        dev.set_configuration()
    except Exception as e:
        print(f"set_configuration: {e}")

    cfg = dev.get_active_configuration()
    intf = cfg[(0,0)]
    ep_out = usb.util.find_descriptor(intf,
        custom_match=lambda e: usb.util.endpoint_direction(e.bEndpointAddress) == usb.util.ENDPOINT_OUT)
    ep_in = usb.util.find_descriptor(intf,
        custom_match=lambda e: usb.util.endpoint_direction(e.bEndpointAddress) == usb.util.ENDPOINT_IN)

    # Read initial packet - THE KEY FIX: use 3 second timeout
    print("\n=== Waiting for HELLO_REQ (3s timeout) ===")
    data = usb_read(3000)

    if not data:
        print("No data received - device already in error state?")
        print("Trying to send RESET_STATE_MACHINE...")
        usb_write(struct.pack("<II", 0x13, 8))
        data = usb_read(3000)

    if not data:
        print("Still no response. Try power-cycling the device.")
        sys.exit(1)

    cmd = struct.unpack_from('<I', data)[0]

    if cmd == SAHARA_HELLO_REQ:
        version = struct.unpack_from('<I', data, 8)[0]
        version_sup = struct.unpack_from('<I', data, 12)[0]
        mode = struct.unpack_from('<I', data, 20)[0]
        print(f"\n*** GOT HELLO_REQ! version={version}, supported={version_sup}, mode={mode} ***")

        # First try command mode to get device info
        hwidstr, pkhash = run_command_mode()

        # Find appropriate loader
        pkhash_prefix = pkhash[:16] if pkhash else None
        loader = find_loader(hwidstr, pkhash_prefix)

        if loader:
            print(f"\n=== Attempting loader upload: {loader} ===")
            # Re-enter command mode first to get fresh state
            # (already in command mode from above, send SWITCH_MODE)
            switch_pkt = struct.pack("<III", SAHARA_SWITCH_MODE, 12, MODE_IMAGE_TX_PENDING)
            usb_write(switch_pkt)
            time.sleep(0.1)

            if not upload_loader(loader):
                print("\nLoader upload failed.")
                print("The device likely requires a ZTE-signed firehose loader.")
                print(f"Look for loader matching: HWID={hwidstr} PK_HASH_PREFIX={pkhash_prefix}")
        else:
            print("\nNo loader found. Device info obtained above is useful for finding the right loader.")

    elif cmd == SAHARA_END_TRANSFER:
        # Already in error state - still try to get info
        image_id, status = struct.unpack_from('<II', data, 8)
        print(f"\nDevice in END_TRANSFER state: image_id={hex(image_id)}, status={hex(status)}")
        print("Device missed HELLO_REQ window. Try power cycling and rerunning.")

    else:
        print(f"\nUnexpected initial response: cmd={hex(cmd)}")
        print(f"Raw: {data.hex()}")

if __name__ == "__main__":
    if os.geteuid() != 0:
        print("This script requires root. Run: sudo python3 edl_catch_hello.py")
        sys.exit(1)
    main()
