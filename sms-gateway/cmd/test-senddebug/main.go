package main

// Verbose SMS send debug: step-by-step with raw output at each stage.
// Run on device as: /system/xbin/librank /data/sms-gateway/test-senddebug

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	fmt.Println("=== SMS Send Debug ===")

	// Find RILD PID
	rildPID := findRILDPid()
	fmt.Printf("RILD PID: %d\n", rildPID)

	// SIGSTOP RILD
	if rildPID > 0 {
		if err := syscall.Kill(rildPID, syscall.SIGSTOP); err != nil {
			fmt.Printf("SIGSTOP failed: %v\n", err)
		} else {
			fmt.Println("RILD stopped")
		}
	}
	defer func() {
		if rildPID > 0 {
			syscall.Kill(rildPID, syscall.SIGCONT)
			fmt.Println("RILD resumed")
		}
	}()

	// Open /dev/smd11
	fmt.Println("Opening /dev/smd11...")
	f, err := os.OpenFile("/dev/smd11", os.O_RDWR, 0600)
	if err != nil {
		fmt.Println("Open failed:", err)
		os.Exit(1)
	}
	defer f.Close()
	fmt.Println("Opened")

	buf := make([]byte, 4096)

	// Drain initial data
	fmt.Println("Draining initial buffer...")
	f.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	total := 0
	for {
		n, _ := f.Read(buf)
		if n == 0 {
			break
		}
		total += n
		fmt.Printf("  drained %d bytes: %q\n", n, string(buf[:n]))
	}
	f.SetReadDeadline(time.Time{})
	fmt.Printf("Drain done (%d bytes total)\n", total)

	// Preflight Ctrl-Z (escape any stuck text mode)
	fmt.Println("\n--- Preflight Ctrl-Z ---")
	f.Write([]byte{0x1a})
	time.Sleep(300 * time.Millisecond)
	f.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		n, _ := f.Read(buf)
		if n == 0 {
			break
		}
		fmt.Printf("  preflight response: %q\n", string(buf[:n]))
	}
	f.SetReadDeadline(time.Time{})

	// AT+CMGF=1
	fmt.Println("\n--- AT+CMGF=1 ---")
	f.Write([]byte("AT+CMGF=1\r\n"))
	var cmgfResp strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, _ := f.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			cmgfResp.WriteString(chunk)
			fmt.Printf("  cmgf chunk: %q\n", chunk)
		}
		if strings.Contains(cmgfResp.String(), "OK") {
			fmt.Println("  Got OK")
			break
		}
	}
	if !strings.Contains(cmgfResp.String(), "OK") {
		fmt.Printf("CMGF timeout, got: %q\n", cmgfResp.String())
		os.Exit(1)
	}

	time.Sleep(100 * time.Millisecond)

	// AT+CMGS
	fmt.Println("\n--- AT+CMGS ---")
	f.Write([]byte("AT+CMGS=\"+447700000001\"\r"))
	var promptResp strings.Builder
	promptDeadline := time.Now().Add(10 * time.Second)
	promptFound := false
	for time.Now().Before(promptDeadline) {
		f.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, _ := f.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			promptResp.WriteString(chunk)
			fmt.Printf("  prompt chunk: %q\n", chunk)
			if strings.Contains(chunk, "> ") || strings.Contains(promptResp.String(), "> ") {
				promptFound = true
				break
			}
		}
	}
	fmt.Printf("  prompt response: %q (found=%v)\n", promptResp.String(), promptFound)
	if !promptFound {
		fmt.Println("No prompt received")
		os.Exit(1)
	}

	// Send text + Ctrl-Z
	fmt.Println("\n--- Sending text + Ctrl-Z ---")
	text := fmt.Sprintf("Debug test %s", time.Now().Format("15:04:05"))
	fmt.Printf("  text: %q\n", text)
	f.Write([]byte(text))
	f.Write([]byte{0x1a})
	fmt.Println("  Ctrl-Z sent")

	// Wait for +CMGS (RILD still stopped)
	fmt.Println("\n--- Waiting for +CMGS (30s, RILD still stopped) ---")
	var finalResp strings.Builder
	finalDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(finalDeadline) {
		f.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := f.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			finalResp.WriteString(chunk)
			fmt.Printf("  got %d bytes: %q\n", n, chunk)
		}
		resp := finalResp.String()
		if strings.Contains(resp, "+CMGS:") || strings.Contains(resp, "ERROR") {
			fmt.Println("  Terminal response received")
			break
		}
	}

	fmt.Printf("\n=== Final response: %q ===\n", finalResp.String())
	if strings.Contains(finalResp.String(), "+CMGS:") {
		fmt.Println("SUCCESS: SMS sent!")
	} else {
		fmt.Println("FAILED: no +CMGS")
	}
}

func findRILDPid() int {
	entries, _ := filepath.Glob("/proc/*/cmdline")
	for _, entry := range entries {
		data, err := os.ReadFile(entry)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "rild") {
			parts := strings.Split(entry, "/")
			if len(parts) >= 3 {
				pid, err := strconv.Atoi(parts[2])
				if err == nil && pid > 1 {
					return pid
				}
			}
		}
	}
	return 0
}
