package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
)

func main() {
	fmt.Println("Opening /dev/smd11 for fast CMGS...")
	f, err := os.OpenFile("/dev/smd11", os.O_RDWR, 0600)
	if err != nil {
		fmt.Println("Open failed:", err)
		os.Exit(1)
	}
	defer func() {
		// Drain and close
		f.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		buf := make([]byte, 4096)
		for {
			n, _ := f.Read(buf)
			if n == 0 {
				break
			}
		}
		f.SetReadDeadline(time.Time{})
		f.Close()
		fmt.Println("Closed and drained")
	}()

	buf := make([]byte, 4096)

	// ESC to cancel text-input
	wr := bufio.NewWriter(f)
	wr.WriteByte(0x1B)
	wr.Flush()
	time.Sleep(300 * time.Millisecond)
	f.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	for {
		n, _ := f.Read(buf)
		if n == 0 {
			break
		}
	}
	f.SetReadDeadline(time.Time{})

	// CMGF=1 — use Write not Printf
	f.Write([]byte("AT+CMGF=1\r\n"))
	time.Sleep(300 * time.Millisecond)

	// Read CMGF OK using fast raw reads
	deadline := time.Now().Add(3 * time.Second)
	var resp []byte
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, _ := f.Read(buf)
		if n > 0 {
			resp = append(resp, buf[:n]...)
		}
	}
	fmt.Printf("CMGF: %q\n", string(resp))

	// CMGS — \r only
	f.Write([]byte("AT+CMGS=\"+447700000001\"\r"))
	time.Sleep(300 * time.Millisecond)

	// Read prompt
	deadline = time.Now().Add(5 * time.Second)
	resp = nil
	gotPrompt := false
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, _ := f.Read(buf)
		if n > 0 {
			resp = append(resp, buf[:n]...)
			if len(resp) >= 2 && resp[len(resp)-2] == '>' && resp[len(resp)-1] == ' ' {
				gotPrompt = true
				break
			}
		}
	}
	fmt.Printf("Prompt: %q (found=%v)\n", string(resp), gotPrompt)

	// Send text + Ctrl-Z
	f.Write([]byte("Fast test"))
	f.Write([]byte{0x1a})

	// Read CMGS response
	deadline = time.Now().Add(30 * time.Second)
	resp = nil
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _ := f.Read(buf)
		if n > 0 {
			resp = append(resp, buf[:n]...)
		}
	}
	fmt.Printf("Final: %q\n", string(resp))
}
