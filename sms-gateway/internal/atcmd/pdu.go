package atcmd

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// gsm7Reverse maps GSM 7-bit code values (0–127) back to Unicode runes.
// Populated at init time from gsm7Table; positions not covered by the table
// fall back to rune(i) (i.e. treat as ASCII) so the common printable range
// 32–122 is always correct.
var gsm7Reverse [128]rune

func init() {
	for i := range gsm7Reverse {
		gsm7Reverse[i] = rune(i)
	}
	for r, b := range gsm7Table {
		if b < 128 {
			gsm7Reverse[b] = r
		}
	}
	// Fill in standard GSM 03.38 positions not present in gsm7Table.
	if gsm7Reverse[27] == rune(27) {
		gsm7Reverse[27] = '\x1B' // ESC / extension table prefix
	}
	if gsm7Reverse[28] == rune(28) {
		gsm7Reverse[28] = 'Æ'
	}
	if gsm7Reverse[29] == rune(29) {
		gsm7Reverse[29] = 'æ'
	}
	if gsm7Reverse[30] == rune(30) {
		gsm7Reverse[30] = 'ß'
	}
	if gsm7Reverse[31] == rune(31) {
		gsm7Reverse[31] = 'É'
	}
}

// gsm7UnpackN extracts n GSM 7-bit septets from data starting at the given
// bit offset (startBit). Used when decoding TP-UD from a PDU, where UDH fill
// bits push the first text septet past bit 0.
func gsm7UnpackN(data []byte, n, startBit int) string {
	var result strings.Builder
	for i := 0; i < n; i++ {
		val := 0
		for bit := 0; bit < 7; bit++ {
			bitPos := startBit + i*7 + bit
			byteIdx := bitPos / 8
			bitOff := uint(bitPos % 8)
			if byteIdx < len(data) && (data[byteIdx]>>bitOff)&1 != 0 {
				val |= 1 << bit
			}
		}
		if val >= 0 && val < 128 {
			result.WriteRune(gsm7Reverse[val])
		}
	}
	return result.String()
}

// decodePDUAddress decodes a TP address field (OA, DA) from raw PDU bytes.
// Returns the decoded number/name string and the number of bytes consumed
// (including the length and type bytes).
func decodePDUAddress(data []byte) (string, int) {
	if len(data) < 2 {
		return "", len(data)
	}
	digitLen := int(data[0])
	addrType := data[1]
	addrBytes := (digitLen + 1) / 2
	total := 2 + addrBytes
	if len(data) < total {
		return "", total
	}

	ton := (addrType >> 4) & 0x07

	var sb strings.Builder
	if ton == 5 { // alphanumeric
		numChars := digitLen * 4 / 7
		if numChars > 0 {
			sb.WriteString(gsm7UnpackN(data[2:total], numChars, 0))
		}
	} else {
		if ton == 1 { // international
			sb.WriteRune('+')
		}
		for i := 0; i < addrBytes; i++ {
			lo := data[2+i] & 0x0F
			hi := (data[2+i] >> 4) & 0x0F
			sb.WriteByte('0' + lo)
			if i*2+1 < digitLen && hi != 0x0F {
				sb.WriteByte('0' + hi)
			}
		}
	}
	return sb.String(), total
}

// decodeSCTS decodes a 7-byte Service Centre Timestamp into a readable string.
// Each byte is BCD with swapped nibbles (tens in low nibble, units in high
// nibble). The timezone byte's bit 3 of the low nibble is the sign flag.
func decodeSCTS(b []byte) string {
	if len(b) < 7 {
		return ""
	}
	bcdSwap := func(x byte) int { return int(x&0x0F)*10 + int((x>>4)&0x0F) }
	yy := bcdSwap(b[0])
	mo := bcdSwap(b[1])
	dd := bcdSwap(b[2])
	hh := bcdSwap(b[3])
	mm := bcdSwap(b[4])
	ss := bcdSwap(b[5])
	// Bit 3 of the low nibble of the timezone byte is the sign (1 = negative).
	tzNeg := (b[6] & 0x08) != 0
	tzRaw := int(b[6]&0x07)*10 + int((b[6]>>4)&0x0F)
	tzSign := "+"
	if tzNeg {
		tzSign = "-"
	}
	tzH := tzRaw / 4
	tzM := (tzRaw % 4) * 15
	return fmt.Sprintf("20%02d/%02d/%02d %02d:%02d:%02d%s%02d%02d",
		yy, mo, dd, hh, mm, ss, tzSign, tzH, tzM)
}

// DecodedPDU holds the fields extracted from a decoded SMS-DELIVER PDU.
type DecodedPDU struct {
	Sender      string
	Timestamp   string
	Body        string
	ConcatRef   int
	ConcatTotal int
	ConcatPart  int
}

// DecodeSMSPDU decodes a full SMS-DELIVER PDU (hex string, including the SMSC
// prefix) into a DecodedPDU. Supports GSM 7-bit (DCS 0x00) and UCS-2
// (DCS 0x08) encodings, with or without a User Data Header for multi-part SMS.
//
// This is the PDU-mode counterpart of the text-mode parseCMGL+parseUDH path.
// Unlike text mode (AT+CMGF=1), PDU mode preserves the UDH bytes so multi-part
// concatenation headers are available for detection.
func DecodeSMSPDU(pduHex string) (*DecodedPDU, error) {
	pduHex = strings.TrimSpace(pduHex)
	raw, err := hex.DecodeString(pduHex)
	if err != nil {
		return nil, fmt.Errorf("hex decode: %w", err)
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("PDU too short: %d bytes", len(raw))
	}

	pos := 0

	// Skip SMSC info: first byte is the length of the SMSC field (in bytes).
	smscLen := int(raw[pos])
	pos += 1 + smscLen
	if pos >= len(raw) {
		return nil, fmt.Errorf("PDU ends after SMSC")
	}

	// First octet: contains TP-MTI (bits 1:0) and TP-UDHI (bit 6).
	firstOctet := raw[pos]
	pos++
	udhi := (firstOctet>>6)&1 != 0

	// Originating Address.
	if pos >= len(raw) {
		return nil, fmt.Errorf("PDU too short at OA")
	}
	sender, oaLen := decodePDUAddress(raw[pos:])
	pos += oaLen

	// PID + DCS.
	if pos+2 > len(raw) {
		return nil, fmt.Errorf("PDU too short at PID/DCS")
	}
	pos++ // skip TP-PID
	dcs := raw[pos]
	pos++

	// Determine character encoding from DCS.
	// Group 0 (bits 7:6 = 00): bits 3:2 give the alphabet.
	// Group F (bits 7:4 = 1111): bit 2 gives the alphabet (0=GSM7, 1=8-bit).
	// Default to GSM7 for anything else.
	var encoding byte // 0=GSM7, 1=8-bit, 2=UCS2
	if dcs&0xC0 == 0 {
		encoding = (dcs >> 2) & 0x03
	} else if dcs&0xF0 == 0xF0 {
		if dcs&0x04 != 0 {
			encoding = 1
		}
	}

	// SCTS (7 bytes).
	if pos+7 > len(raw) {
		return nil, fmt.Errorf("PDU too short at SCTS")
	}
	timestamp := decodeSCTS(raw[pos : pos+7])
	pos += 7

	// UDL + UD.
	if pos >= len(raw) {
		return nil, fmt.Errorf("PDU too short at UDL")
	}
	udl := int(raw[pos])
	pos++
	ud := raw[pos:]

	var body string
	var concatRef, concatTotal, concatPart int

	if udhi {
		if len(ud) < 1 {
			return nil, fmt.Errorf("UDHI set but UD is empty")
		}
		udhl := int(ud[0])
		if len(ud) < 1+udhl {
			return nil, fmt.Errorf("UDHI: UD too short for UDHL=%d", udhl)
		}
		udhContent := ud[1 : 1+udhl]

		// Walk IEs within the UDH looking for concat headers.
		for i := 0; i+2 <= len(udhContent); {
			iei := udhContent[i]
			ielen := int(udhContent[i+1])
			if i+2+ielen > len(udhContent) {
				break
			}
			iedata := udhContent[i+2 : i+2+ielen]
			i += 2 + ielen

			switch iei {
			case 0x00: // 8-bit concat reference (IEI 00, LEN 03)
				if len(iedata) >= 3 {
					concatRef = int(iedata[0])
					concatTotal = int(iedata[1])
					concatPart = int(iedata[2])
				}
			case 0x08: // 16-bit concat reference (IEI 08, LEN 04)
				if len(iedata) >= 4 {
					concatRef = (int(iedata[0]) << 8) | int(iedata[1])
					concatTotal = int(iedata[2])
					concatPart = int(iedata[3])
				}
			}
		}

		udhTotalBytes := 1 + udhl // UDHL byte + UDH content

		switch encoding {
		case 2: // UCS-2: byte-aligned, no fill bits
			if udhTotalBytes < len(ud) {
				body = decodeUCS2BE(ud[udhTotalBytes:])
			}
		case 1: // 8-bit data: byte-aligned, no fill bits
			if udhTotalBytes < len(ud) {
				body = decodeLatin1(ud[udhTotalBytes:])
			}
		default: // GSM7: fill bits align UDH to septet boundary
			// fillBits pads the UDH to the next multiple of 7 bits so the
			// first text septet starts on a septet boundary.
			fillBits := (7 - (udhTotalBytes % 7)) % 7
			// UDL counts total septets including the "virtual" UDH septets.
			udhSeptets := (udhTotalBytes*8 + fillBits) / 7
			bodyChars := udl - udhSeptets
			if bodyChars > 0 {
				startBit := udhTotalBytes*8 + fillBits
				body = gsm7UnpackN(ud, bodyChars, startBit)
			}
		}
	} else {
		// No UDH — decode the entire UD as body.
		switch encoding {
		case 2: // UCS-2
			if udl <= len(ud) {
				body = decodeUCS2BE(ud[:udl])
			} else {
				body = decodeUCS2BE(ud)
			}
		case 1: // 8-bit data
			if udl <= len(ud) {
				body = decodeLatin1(ud[:udl])
			} else {
				body = decodeLatin1(ud)
			}
		default: // GSM7
			body = gsm7UnpackN(ud, udl, 0)
		}
	}

	return &DecodedPDU{
		Sender:      sender,
		Timestamp:   timestamp,
		Body:        body,
		ConcatRef:   concatRef,
		ConcatTotal: concatTotal,
		ConcatPart:  concatPart,
	}, nil
}

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
