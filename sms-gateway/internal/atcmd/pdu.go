package atcmd

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// gsm7Table maps basic GSM 7-bit character set. Characters not in this table
// fall back to their ASCII value if it falls within 7-bit range.
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

// toGSM7 maps a rune to its GSM 7-bit value. Unknown characters map to '?'.
func toGSM7(r rune) byte {
	if v, ok := gsm7Table[r]; ok {
		return v
	}
	// ASCII printable range is also valid in GSM 7-bit basic charset
	if r >= 32 && r <= 126 {
		if v, ok := gsm7Table[r]; ok {
			return v
		}
		return byte(r)
	}
	return 63 // '?' fallback
}

// gsm7Pack packs a string into GSM 7-bit encoded bytes.
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

// encodeSMSPDU builds a TP-SUBMIT PDU (WITHOUT the leading SMSC length byte)
// for use with Android's RIL_REQUEST_SEND_SMS. Returns the PDU as a hex string.
//
// Only GSM 7-bit encoding is supported. Messages must be <= 160 characters.
func encodeSMSPDU(number, text string) (string, error) {
	if len([]rune(text)) > 160 {
		return "", fmt.Errorf("message too long: %d chars (max 160)", len([]rune(text)))
	}

	// Normalise number: strip leading +
	digits := strings.TrimPrefix(number, "+")
	numLen := len(digits)
	if numLen == 0 || numLen > 20 {
		return "", fmt.Errorf("invalid phone number: %q", number)
	}

	var pdu []byte

	// Byte 0: First PDU octet
	//   MTI  = 01 (SMS-SUBMIT)
	//   RD   = 0
	//   VPF  = 10 (relative validity period present)
	//   SRR  = 0
	//   UDHI = 0
	//   RP   = 0
	pdu = append(pdu, 0x11)

	// Byte 1: TP-MR (message reference, 0 = modem assigns)
	pdu = append(pdu, 0x00)

	// Destination address
	pdu = append(pdu, byte(numLen)) // TP-DA length in digits
	if number[0] == '+' {
		pdu = append(pdu, 0x91) // TON=international, NPI=ISDN
	} else {
		pdu = append(pdu, 0x81) // TON=unknown, NPI=ISDN
	}
	// BCD encode digits (first digit in low nibble of each byte)
	for i := 0; i < numLen; i += 2 {
		lo := digits[i] - '0'
		var hi byte
		if i+1 < numLen {
			hi = (digits[i+1] - '0') << 4
		} else {
			hi = 0xF0 // padding for odd-length numbers
		}
		pdu = append(pdu, hi|lo)
	}

	// TP-PID: 0x00 (normal)
	pdu = append(pdu, 0x00)

	// TP-DCS: 0x00 (GSM 7-bit default alphabet)
	pdu = append(pdu, 0x00)

	// TP-VP (relative): 0xA7 = 24 hours
	pdu = append(pdu, 0xA7)

	// TP-UDL: user data length in septets (characters)
	runes := []rune(text)
	pdu = append(pdu, byte(len(runes)))

	// TP-UD: GSM 7-bit packed
	pdu = append(pdu, gsm7Pack(text)...)

	return strings.ToUpper(hex.EncodeToString(pdu)), nil
}
