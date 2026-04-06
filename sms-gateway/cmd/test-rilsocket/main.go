package main

// test-rilsocket: diagnose RIL socket connectivity and RIL_REQUEST_SEND_SMS.
// Run on device as: /system/xbin/librank /data/sms-gateway/test-rilsocket <number> <text>

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"time"
)

const rilRequestSendSMS = 25

func main() {
	number := "+447700000001"
	text := "RIL socket test"
	if len(os.Args) >= 2 {
		number = os.Args[1]
	}
	if len(os.Args) >= 3 {
		text = os.Args[2]
	}

	fmt.Printf("=== RIL Socket SMS Test ===\n")
	fmt.Printf("To: %s\n", number)
	fmt.Printf("Text: %q\n\n", text)

	// 1. PDU encode
	pdu, err := encodeSMSPDU(number, text)
	if err != nil {
		fmt.Printf("PDU encode error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PDU: %s\n\n", pdu)

	// 2. Connect
	fmt.Println("Connecting to /dev/socket/rild...")
	conn, err := net.DialTimeout("unix", "/dev/socket/rild", 3*time.Second)
	if err != nil {
		fmt.Printf("FAIL connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Println("Connected!")

	// 3. Read initial unsolicited notification (radio state) — RILD sends this on connect
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var lenBuf [4]byte
	if err := readFull(conn, lenBuf[:]); err == nil {
		pktLen := binary.BigEndian.Uint32(lenBuf[:])
		fmt.Printf("Initial packet: %d bytes\n", pktLen)
		if pktLen > 0 && pktLen <= 65536 {
			pkt := make([]byte, pktLen)
			if err := readFull(conn, pkt); err == nil {
				if len(pkt) >= 4 {
					pktType := binary.LittleEndian.Uint32(pkt[0:4])
					fmt.Printf("  type=%d (0=response, 1=unsolicited)\n", pktType)
					if len(pkt) >= 8 {
						unsolID := binary.LittleEndian.Uint32(pkt[4:8])
						fmt.Printf("  id=%d\n", unsolID)
					}
					fmt.Printf("  hex: %s\n", hex.EncodeToString(pkt[:min(pkt, 64)]))
				}
			}
		}
	}
	conn.SetReadDeadline(time.Time{})

	// 4. Build and send request
	const serial uint32 = 42
	var body []byte
	body = appendU32(body, rilRequestSendSMS)
	body = appendU32(body, int(serial))
	body = appendNullStr(body)        // SMSC = null
	body = appendStr(body, pdu)       // PDU

	msg := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(msg[:4], uint32(len(body)))
	copy(msg[4:], body)

	fmt.Printf("\nSending RIL_REQUEST_SEND_SMS (code=%d, serial=%d)\n", rilRequestSendSMS, serial)
	fmt.Printf("Request hex (%d bytes): %s\n\n", len(msg), hex.EncodeToString(msg))

	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		fmt.Printf("FAIL write: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Request sent, waiting for response...")

	// 5. Read responses
	for i := 0; i < 10; i++ {
		var lb [4]byte
		if err := readFull(conn, lb[:]); err != nil {
			fmt.Printf("Read length err: %v\n", err)
			break
		}
		pktLen := binary.BigEndian.Uint32(lb[:])
		fmt.Printf("\n--- Packet %d: %d bytes ---\n", i+1, pktLen)
		if pktLen == 0 || pktLen > 65536 {
			fmt.Printf("Implausible length, stopping\n")
			break
		}
		pkt := make([]byte, pktLen)
		if err := readFull(conn, pkt); err != nil {
			fmt.Printf("Read body err: %v\n", err)
			break
		}
		fmt.Printf("Hex: %s\n", hex.EncodeToString(pkt))
		if len(pkt) >= 12 {
			pktType := binary.LittleEndian.Uint32(pkt[0:4])
			pktSerial := binary.LittleEndian.Uint32(pkt[4:8])
			rilErr := binary.LittleEndian.Uint32(pkt[8:12])
			fmt.Printf("type=%d serial=%d rilErr=%d\n", pktType, pktSerial, rilErr)
			if pktType == 0 && pktSerial == serial {
				if rilErr != 0 {
					fmt.Printf("FAIL: RIL error %d\n", rilErr)
				} else {
					if len(pkt) >= 16 {
						msgRef := binary.LittleEndian.Uint32(pkt[12:16])
						fmt.Printf("SUCCESS: messageRef=%d\n", msgRef)
					} else {
						fmt.Printf("SUCCESS: (no messageRef in response)\n")
					}
				}
				return
			}
		}
	}
	fmt.Println("\nNo matching response received.")
}

func min(data []byte, n int) int {
	if len(data) < n {
		return len(data)
	}
	return n
}

func appendU32(b []byte, v int) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(v))
	return append(b, buf[:]...)
}

func appendNullStr(b []byte) []byte {
	return append(b, 0xFF, 0xFF, 0xFF, 0xFF)
}

func appendStr(b []byte, s string) []byte {
	runes := []rune(s)
	charCount := len(runes)
	b = appendU32(b, charCount)
	for _, r := range runes {
		b = append(b, byte(r&0xFF), byte(r>>8))
	}
	b = append(b, 0, 0) // null terminator
	written := (charCount + 1) * 2
	if pad := (4 - written%4) % 4; pad > 0 {
		for i := 0; i < pad; i++ {
			b = append(b, 0)
		}
	}
	return b
}

func readFull(conn net.Conn, buf []byte) error {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			if total == len(buf) {
				return nil
			}
			return err
		}
	}
	return nil
}

// encodeSMSPDU builds a TP-SUBMIT PDU (without leading SMSC byte) for
// RIL_REQUEST_SEND_SMS. Returns the PDU as an uppercase hex string.
func encodeSMSPDU(number, text string) (string, error) {
	if len([]rune(text)) > 160 {
		return "", fmt.Errorf("message too long")
	}
	digits := number
	if len(digits) > 0 && digits[0] == '+' {
		digits = digits[1:]
	}
	if len(digits) == 0 || len(digits) > 20 {
		return "", fmt.Errorf("invalid phone number")
	}
	isIntl := len(number) > 0 && number[0] == '+'

	var pdu []byte
	pdu = append(pdu, 0x11) // MTI=SUBMIT, VPF=relative
	pdu = append(pdu, 0x00) // TP-MR
	pdu = append(pdu, byte(len(digits)))
	if isIntl {
		pdu = append(pdu, 0x91)
	} else {
		pdu = append(pdu, 0x81)
	}
	for i := 0; i < len(digits); i += 2 {
		lo := digits[i] - '0'
		var hi byte
		if i+1 < len(digits) {
			hi = (digits[i+1] - '0') << 4
		} else {
			hi = 0xF0
		}
		pdu = append(pdu, hi|lo)
	}
	pdu = append(pdu, 0x00) // PID
	pdu = append(pdu, 0x00) // DCS = GSM7
	pdu = append(pdu, 0xA7) // VP = 24h
	runes := []rune(text)
	pdu = append(pdu, byte(len(runes)))
	pdu = append(pdu, gsm7Pack(text)...)

	result := make([]byte, hex.EncodedLen(len(pdu)))
	hex.Encode(result, pdu)
	for i, c := range result {
		if c >= 'a' && c <= 'f' {
			result[i] = c - 32
		}
	}
	return string(result), nil
}

func gsm7Pack(text string) []byte {
	if len(text) == 0 {
		return nil
	}
	runes := []rune(text)
	chars := make([]byte, len(runes))
	for i, r := range runes {
		chars[i] = toGSM7(r)
	}
	totalBits := len(chars) * 7
	out := make([]byte, (totalBits+7)/8)
	for i, c := range chars {
		bitPos := i * 7
		bytePos := bitPos / 8
		bitOffset := bitPos % 8
		out[bytePos] |= c << bitOffset
		if bitOffset > 1 {
			out[bytePos+1] |= c >> (8 - bitOffset)
		}
	}
	return out
}

var gsm7Table = map[rune]byte{
	'@': 0, '£': 1, '$': 2, '¥': 3, 'è': 4, 'é': 5, 'ù': 6, 'ì': 7,
	'ò': 8, 'Ç': 9, '\n': 10, 'Ø': 11, 'ø': 12, '\r': 13, 'Å': 14, 'å': 15,
	'Δ': 16, '_': 17, 'Φ': 18, 'Γ': 19, 'Λ': 20, 'Ω': 21, 'Π': 22, 'Ψ': 23,
	'Σ': 24, 'Θ': 25, 'Ξ': 26, ' ': 32, '!': 33, '"': 34, '#': 35,
	'¤': 36, '%': 37, '&': 38, '\'': 39, '(': 40, ')': 41, '*': 42, '+': 43,
	',': 44, '-': 45, '.': 46, '/': 47,
	'0': 48, '1': 49, '2': 50, '3': 51, '4': 52, '5': 53, '6': 54, '7': 55,
	'8': 56, '9': 57, ':': 58, ';': 59, '<': 60, '=': 61, '>': 62, '?': 63,
	'¡': 64,
	'A': 65, 'B': 66, 'C': 67, 'D': 68, 'E': 69, 'F': 70, 'G': 71, 'H': 72,
	'I': 73, 'J': 74, 'K': 75, 'L': 76, 'M': 77, 'N': 78, 'O': 79, 'P': 80,
	'Q': 81, 'R': 82, 'S': 83, 'T': 84, 'U': 85, 'V': 86, 'W': 87, 'X': 88,
	'Y': 89, 'Z': 90, 'Ä': 91, 'Ö': 92, 'Ñ': 93, 'Ü': 94, '§': 95,
	'¿': 96,
	'a': 97, 'b': 98, 'c': 99, 'd': 100, 'e': 101, 'f': 102, 'g': 103, 'h': 104,
	'i': 105, 'j': 106, 'k': 107, 'l': 108, 'm': 109, 'n': 110, 'o': 111, 'p': 112,
	'q': 113, 'r': 114, 's': 115, 't': 116, 'u': 117, 'v': 118, 'w': 119, 'x': 120,
	'y': 121, 'z': 122, 'ä': 123, 'ö': 124, 'ñ': 125, 'ü': 126, 'à': 127,
}

func toGSM7(r rune) byte {
	if v, ok := gsm7Table[r]; ok {
		return v
	}
	if r >= 32 && r <= 126 {
		return byte(r)
	}
	return 63
}
