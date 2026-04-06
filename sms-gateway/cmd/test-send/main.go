package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	fmt.Println("=== Direct Script SMS Test ===")

	// Write the script directly (no Go session at all)
	script := `#!/system/bin/sh
BB=/system/bin/busybox
DEV=/dev/smd11
LOG=/data/sms-gateway/_sms_send.log

exec 3<>$DEV
# Set text mode
$BB printf 'AT+CMGF=1\r\n' >&3; sleep 2
$BB dd if=$DEV bs=512 count=1 2>/dev/null
# Set character set
$BB printf 'AT+CSCS="IRA"\r\n' >&3; sleep 1
$BB dd if=$DEV bs=512 count=1 2>/dev/null
# Send number
$BB printf 'AT+CMGS="+447700000001"\r' >&3; sleep 3
$BB dd if=$DEV bs=256 count=1 2>/dev/null
# Send message + Ctrl-Z
$BB printf 'Direct script test %s' >&3
$BB printf '\x1a' >&3; sleep 8
# Capture response
$BB dd if=$DEV bs=4096 count=1 >$LOG 2>/dev/null
exec 3>&-
`
	script = fmt.Sprintf(script, time.Now().Format("15:04:05"))

	if err := os.WriteFile("/data/sms-gateway/_direct_test.sh", []byte(script), 0755); err != nil {
		fmt.Printf("Write error: %v\n", err)
		os.Exit(1)
	}

	// Run via librank (clean process, no inherited fds)
	cmd := exec.Command("/system/xbin/librank", "/system/bin/sh", "/data/sms-gateway/_direct_test.sh")
	output, err := cmd.CombinedOutput()
	fmt.Printf("Script output: %s\n", string(output))
	if err != nil {
		fmt.Printf("Exec error: %v\n", err)
	}

	// Read log
	logData, _ := os.ReadFile("/data/sms-gateway/_sms_send.log")
	fmt.Printf("Log file:\n%s\n", string(logData))

	if len(logData) > 0 {
		fmt.Println("SUCCESS - script ran and got a response")
	} else {
		fmt.Println("EMPTY LOG - script may have failed")
	}
}
