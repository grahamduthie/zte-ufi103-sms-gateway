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
		fmt.Printf("Drained %d bytes\n", n)
	}
	f.SetReadDeadline(time.Time{})
	fmt.Println("ESC drain done")

	// Write CMGF
	fmt.Println("Writing AT+CMGF=1...")
	_, err = f.Write([]byte("AT+CMGF=1\r\n"))
	if err != nil {
		fmt.Println("Write failed:", err)
		os.Exit(1)
	}
	fmt.Println("Written")

	// Read response
	fmt.Println("Reading response...")
	deadline := time.Now().Add(5 * time.Second)
	var response []byte
	for time.Now().Before(deadline) {
		f.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := f.Read(buf)
		if n > 0 {
			response = append(response, buf[:n]...)
			fmt.Printf("Read %d bytes: %q\n", n, string(buf[:n]))
		}
		if err != nil {
			continue
		}
	}
	fmt.Printf("Total response: %q (%d bytes)\n", string(response), len(response))
}
