package atcmd

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

const rilRequestSendSMS = 25

// sendSMSViaRIL sends an SMS through RILD's Unix domain socket using the
// Android Parcel binary protocol. This avoids the /dev/smd11 contention
// where RILD's persistent read() syscall consumes all modem responses.
//
// Protocol (each field is little-endian unless noted):
//
//	Request:  [4B BE: body length] [4B LE: request code] [4B LE: serial] [Parcel string: SMSC] [Parcel string: PDU]
//	Response: [4B BE: body length] [4B LE: type=0] [4B LE: serial] [4B LE: RIL error] [4B LE: messageRef] ...
//
// Parcel string: [4B LE: char count, or -1 for null] [UTF-16LE chars + null] [4-byte-aligned padding]
func sendSMSViaRIL(number, text string) (int, error) {
	pdu, err := encodeSMSPDU(number, text)
	if err != nil {
		return 0, fmt.Errorf("PDU encode: %w", err)
	}

	const serial uint32 = 1

	// Build request Parcel body.
	var body []byte
	body = parcelAppendU32(body, rilRequestSendSMS)     // request code
	body = parcelAppendU32(body, int(serial))           // serial
	body = parcelAppendNullString(body)                 // SMSC: null → modem uses stored value
	body = parcelAppendString(body, pdu)                // PDU hex string

	// Prepend 4-byte big-endian length.
	msg := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(msg[:4], uint32(len(body)))
	copy(msg[4:], body)

	conn, err := net.DialTimeout("unix", "/dev/socket/rild", 3*time.Second)
	if err != nil {
		return 0, fmt.Errorf("connect /dev/socket/rild: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	if _, err := conn.Write(msg); err != nil {
		return 0, fmt.Errorf("write RIL request: %w", err)
	}

	// Read responses until we find one matching our serial.
	// RILD may send unsolicited notifications interleaved with our response.
	pktCount := 0
	for {
		pktCount++
		if pktCount > 50 {
			return 0, fmt.Errorf("too many packets (%d) without matching response", pktCount)
		}

		var lenBuf [4]byte
		if err := rilReadFull(conn, lenBuf[:]); err != nil {
			return 0, fmt.Errorf("read response length: %w", err)
		}
		respLen := binary.BigEndian.Uint32(lenBuf[:])
		if respLen == 0 || respLen > 65536 {
			return 0, fmt.Errorf("implausible RIL response length: %d", respLen)
		}

		pkt := make([]byte, respLen)
		if err := rilReadFull(conn, pkt); err != nil {
			return 0, fmt.Errorf("read response body: %w", err)
		}
		if len(pkt) < 12 {
			continue
		}

		pktType := binary.LittleEndian.Uint32(pkt[0:4])
		pktSerial := binary.LittleEndian.Uint32(pkt[4:8])

		if pktType != 0 {
			continue // unsolicited notification
		}

		if pktSerial != serial {
			continue // different serial
		}

		rilError := binary.LittleEndian.Uint32(pkt[8:12])
		if rilError != 0 {
			return 0, fmt.Errorf("RIL_REQUEST_SEND_SMS failed with RIL error %d", rilError)
		}

		if len(pkt) < 16 {
			return 0, fmt.Errorf("response too short for messageRef: %d bytes", len(pkt))
		}
		messageRef := int(binary.LittleEndian.Uint32(pkt[12:16]))
		return messageRef, nil
	}
}

func parcelAppendU32(b []byte, v int) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(v))
	return append(b, buf[:]...)
}

func parcelAppendNullString(b []byte) []byte {
	// Null string is encoded as -1 (0xFFFFFFFF LE).
	return append(b, 0xFF, 0xFF, 0xFF, 0xFF)
}

func parcelAppendString(b []byte, s string) []byte {
	runes := []rune(s)
	charCount := len(runes)
	b = parcelAppendU32(b, charCount)
	// UTF-16LE characters followed by null terminator.
	for _, r := range runes {
		// All our PDU strings are pure ASCII, so r always fits in 1 UTF-16 code unit.
		b = append(b, byte(r&0xFF), byte(r>>8))
	}
	b = append(b, 0x00, 0x00) // null terminator
	// Pad to 4-byte boundary. Bytes written = (charCount+1)*2.
	written := (charCount + 1) * 2
	if pad := (4 - written%4) % 4; pad > 0 {
		for i := 0; i < pad; i++ {
			b = append(b, 0x00)
		}
	}
	return b
}

func rilReadFull(conn net.Conn, buf []byte) error {
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
