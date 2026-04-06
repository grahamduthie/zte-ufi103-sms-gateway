package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
)

func main() {
	fmt.Println("Opening /dev/smd11...")
	f, err := os.OpenFile("/dev/smd11", os.O_RDWR, 0600)
	if err != nil {
		fmt.Println("Open failed:", err)
		os.Exit(1)
	}
	defer f.Close()
	fmt.Println("Opened successfully")

	// ESC to cancel text-input
	wr := bufio.NewWriter(f)
	wr.WriteByte(0x1B)
	wr.Flush()
	fmt.Println("ESC sent")

	time.Sleep(500 * time.Millisecond)

	// Drain
	f.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buf := make([]byte, 4096)
	for {
		n, _ := f.Read(buf)
		if n == 0 {
			break
		}
	}
	f.SetReadDeadline(time.Time{})
	fmt.Println("ESC drain done")

	// CMGF=1
	fmt.Println("Writing CMGF=1...")
	f.Write([]byte("AT+CMGF=1\r\n"))
	time.Sleep(200 * time.Millisecond)

	// Read OK
	fmt.Println("Reading CMGF OK...")
	deadline := time.Now().Add(5 * time.Second)
	var resp []byte
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := f.Read(buf)
		if n > 0 {
			resp = append(resp, buf[:n]...)
		}
		if err != nil {
			continue
		}
	}
	if len(resp) == 0 {
		fmt.Println("CMGF: NO RESPONSE")
		os.Exit(1)
	}
	fmt.Printf("CMGF response: %q\n", string(resp))

	// Wait before CMGS
	time.Sleep(500 * time.Millisecond)

	// CMGS
	fmt.Println("Writing CMGS...")
	f.Write([]byte("AT+CMGS=\"+447700000001\"\r"))
	time.Sleep(500 * time.Millisecond)

	// Read prompt
	fmt.Println("Reading prompt...")
	deadline = time.Now().Add(5 * time.Second)
	resp = nil
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := f.Read(buf)
		if n > 0 {
			resp = append(resp, buf[:n]...)
			if len(resp) > 0 && resp[len(resp)-1] == ' ' && len(resp) >= 2 && resp[len(resp)-2] == '>' {
				fmt.Println("Found '> ' prompt!")
				break
			}
		}
		if err != nil {
			continue
		}
	}
	fmt.Printf("Prompt response: %q\n", string(resp))

	// Send text + Ctrl-Z
	fmt.Println("Sending text + Ctrl-Z...")
	f.Write([]byte("Hello from minimal test"))
	f.Write([]byte{0x1a})

	// Read response
	fmt.Println("Reading CMGS response...")
	deadline = time.Now().Add(30 * time.Second)
	resp = nil
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := f.Read(buf)
		if n > 0 {
			resp = append(resp, buf[:n]...)
			fmt.Printf("Read %d: %q\n", n, string(buf[:n]))
		}
		if err != nil {
			continue
		}
	}
	fmt.Printf("Final CMGS response: %q\n", string(resp))
}
