package atcmd

import (
	"strings"
	"testing"
)

// ── toGSM7 tests ─────────────────────────────────────────────────────────

func TestToGSM7_BasicASCII(t *testing.T) {
	// Test individual character mappings for common ASCII chars
	testCases := []struct {
		input    rune
		expected byte
	}{
		{'A', 0x41},
		{'B', 0x42},
		{'a', 0x61},
		{'0', 0x30},
		{' ', 0x20},
		{'!', 0x21},
		{'\n', 0x0A},
		{'\r', 0x0D},
	}

	for _, tc := range testCases {
		result := toGSM7(tc.input)
		if result != tc.expected {
			t.Errorf("toGSM7(%q) = 0x%02X, expected 0x%02X", tc.input, result, tc.expected)
		}
	}
}

func TestToGSM7_ExtendedChars(t *testing.T) {
	// toGSM7 returns the direct ASCII value for extended chars.
	// Actual PDU encoding would prefix these with ESC (0x1B), but
	// this function just maps the rune directly.
	testCases := []struct {
		input    rune
		expected byte
	}{
		{'^', '^'},  // 0x5E
		{'{', '{'},  // 0x7B
		{'}', '}'},  // 0x7D
		{'[', '['},  // 0x5B
		{']', ']'},  // 0x5D
		{'~', '~'},  // 0x7E
	}

	for _, tc := range testCases {
		result := toGSM7(tc.input)
		if result != tc.expected {
			t.Errorf("toGSM7(%q) = 0x%02X, expected 0x%02X", tc.input, result, tc.expected)
		}
	}
}

func TestToGSM7_NonGSMChar(t *testing.T) {
	// Emoji is not in GSM charset → should be replaced with '?'
	result := toGSM7('🌍')
	if result != 0x3F {
		t.Errorf("toGSM7('🌍') = 0x%02X, expected 0x3F ('?')", result)
	}
}

// ── gsm7Pack tests ───────────────────────────────────────────────────────

func TestGSM7Pack_Simple(t *testing.T) {
	result := gsm7Pack("Hello")
	if len(result) == 0 {
		t.Fatal("gsm7Pack returned empty")
	}
	// "Hello" = 5 chars * 7 bits = 35 bits → 5 bytes
	if len(result) != 5 {
		t.Fatalf("expected 5 bytes, got %d", len(result))
	}
}

func TestGSM7Pack_Empty(t *testing.T) {
	result := gsm7Pack("")
	if len(result) != 0 {
		t.Fatalf("expected empty result, got %v", result)
	}
}

func TestGSM7Pack_Padding(t *testing.T) {
	// 7 chars → 7*7 = 49 bits → 7 bytes (56 bits, padded with 0x00)
	result := gsm7Pack("ABCDEFG")
	expectedLen := (7*7 + 7) / 8 // 49 bits → 7 bytes
	if len(result) != expectedLen {
		t.Fatalf("expected %d bytes, got %d", expectedLen, len(result))
	}
}

func TestGSM7Pack_EightChars(t *testing.T) {
	// 8 chars → 8*7 = 56 bits → 7 bytes exactly (no padding)
	result := gsm7Pack("ABCDEFGH")
	expectedLen := (8*7 + 7) / 8 // 56 bits → 7 bytes
	if len(result) != expectedLen {
		t.Fatalf("expected %d bytes, got %d", expectedLen, len(result))
	}
}

// ── encodeSMSPDU tests ───────────────────────────────────────────────────

func TestEncodeSMSPDU_Basic(t *testing.T) {
	number := "+447700000001"
	text := "Hello"
	pdu, err := encodeSMSPDU(number, text)
	if err != nil {
		t.Fatalf("encodeSMSPDU failed: %v", err)
	}
	if len(pdu) == 0 {
		t.Fatal("encodeSMSPDU returned empty PDU")
	}
	// PDU should be valid hex
	for _, c := range pdu {
		if !isHexChar(byte(c)) {
			t.Fatalf("PDU contains non-hex char %q (full PDU: %s)", c, pdu)
		}
	}
}

func TestEncodeSMSPDU_WithSpaces(t *testing.T) {
	number := "+447700000001"
	text := "Hello World"
	pdu, err := encodeSMSPDU(number, text)
	if err != nil {
		t.Fatalf("encodeSMSPDU with spaces failed: %v", err)
	}
	if len(pdu) == 0 {
		t.Fatal("encodeSMSPDU returned empty PDU")
	}
}

func TestEncodeSMSPDU_EmptyText(t *testing.T) {
	number := "+447700000001"
	text := ""
	pdu, err := encodeSMSPDU(number, text)
	if err != nil {
		t.Fatalf("encodeSMSPDU with empty text failed: %v", err)
	}
	if len(pdu) == 0 {
		t.Fatal("encodeSMSPDU returned empty PDU for empty text")
	}
}

func TestEncodeSMSPDU_LongText(t *testing.T) {
	number := "+447700000001"
	// 160 chars — max for single SMS
	text := strings.Repeat("A", 160)
	pdu, err := encodeSMSPDU(number, text)
	if err != nil {
		t.Fatalf("encodeSMSPDU with 160 chars failed: %v", err)
	}
	if len(pdu) == 0 {
		t.Fatal("encodeSMSPDU returned empty PDU for 160 chars")
	}
}

func TestEncodeSMSPDU_NumberWithoutPlus(t *testing.T) {
	number := "447700000001"
	text := "Test"
	pdu, err := encodeSMSPDU(number, text)
	if err != nil {
		t.Fatalf("encodeSMSPDU without + failed: %v", err)
	}
	if len(pdu) == 0 {
		t.Fatal("encodeSMSPDU returned empty PDU")
	}
}

func TestEncodeSMSPDU_InternationalNumber(t *testing.T) {
	number := "+12125551234"
	text := "US number test"
	pdu, err := encodeSMSPDU(number, text)
	if err != nil {
		t.Fatalf("encodeSMSPDU with US number failed: %v", err)
	}
	if len(pdu) == 0 {
		t.Fatal("encodeSMSPDU returned empty PDU")
	}
}

// ── DecodeSMSPDU tests ───────────────────────────────────────────────────

// Verified PDUs — constructed manually and cross-checked against the decoder.
//
// Anatomy of a PDU used in these tests (bytes in order):
//   [00]           SMSC length=0 (no SMSC)
//   [04]           first octet: SMS-DELIVER, no UDHI
//   [0C]           OA length: 12 digits
//   [91]           OA type: international
//   [44 77 00 00 00 10]  OA BCD: +447700000001
//   [00]           PID
//   [XX]           DCS
//   [62 40 51 21 00 00 00]  SCTS: 2026-04-15 12:00:00 +0000
//   [NN]           UDL
//   [...]          UD

// TestDecodeSMSPDU_SinglePartGSM7 checks a two-character GSM7 message.
func TestDecodeSMSPDU_SinglePartGSM7(t *testing.T) {
	// "Hi" from +447700000001 — GSM7, no UDH
	// H=72 (0x48), i=105 (0x69)
	// Packed: byte0 = 0xC8 (H bits + i.bit0), byte1 = 0x34 (i.bits1-6)
	pdu := "00040C914477000000100000624051210000000 2C834"
	pdu = strings.ReplaceAll(pdu, " ", "")
	dec, err := DecodeSMSPDU(pdu)
	if err != nil {
		t.Fatalf("DecodeSMSPDU: %v", err)
	}
	if dec.Sender != "+447700000001" {
		t.Errorf("Sender: got %q, want %q", dec.Sender, "+447700000001")
	}
	if dec.Body != "Hi" {
		t.Errorf("Body: got %q, want %q", dec.Body, "Hi")
	}
	if dec.ConcatRef != 0 || dec.ConcatTotal != 0 || dec.ConcatPart != 0 {
		t.Errorf("unexpected concat fields: ref=%d total=%d part=%d",
			dec.ConcatRef, dec.ConcatTotal, dec.ConcatPart)
	}
}

// TestDecodeSMSPDU_MultiPartGSM7 checks that an 8-bit concat UDH is decoded.
// Body "ABC" (GSM7 codes 65,66,67) with UDH [05 00 03 2A 02 01]:
//   ref=42, total=2, part=1.
// UDH is 6 bytes → fillBits=1, startBit=49, udhSeptets=7, UDL=10.
func TestDecodeSMSPDU_MultiPartGSM7(t *testing.T) {
	// first octet = 0x44: SMS-DELIVER + UDHI bit (bit 6) + TP-MMS bit (bit 2)
	pdu := "00440C914477000000100000624051210000000A0500032A020182C221"
	dec, err := DecodeSMSPDU(pdu)
	if err != nil {
		t.Fatalf("DecodeSMSPDU: %v", err)
	}
	if dec.Sender != "+447700000001" {
		t.Errorf("Sender: got %q, want %q", dec.Sender, "+447700000001")
	}
	if dec.Body != "ABC" {
		t.Errorf("Body: got %q, want %q", dec.Body, "ABC")
	}
	if dec.ConcatRef != 42 {
		t.Errorf("ConcatRef: got %d, want 42", dec.ConcatRef)
	}
	if dec.ConcatTotal != 2 {
		t.Errorf("ConcatTotal: got %d, want 2", dec.ConcatTotal)
	}
	if dec.ConcatPart != 1 {
		t.Errorf("ConcatPart: got %d, want 1", dec.ConcatPart)
	}
}

// TestDecodeSMSPDU_UCS2 checks a UCS-2 encoded message (DCS=0x08).
// "Hi" as UCS-2 BE: 00 48 00 69 (4 bytes), UDL=4.
func TestDecodeSMSPDU_UCS2(t *testing.T) {
	pdu := "00040C914477000000100008624051210000000400480069"
	dec, err := DecodeSMSPDU(pdu)
	if err != nil {
		t.Fatalf("DecodeSMSPDU: %v", err)
	}
	if dec.Sender != "+447700000001" {
		t.Errorf("Sender: got %q, want %q", dec.Sender, "+447700000001")
	}
	if dec.Body != "Hi" {
		t.Errorf("Body: got %q, want %q", dec.Body, "Hi")
	}
	if dec.ConcatRef != 0 {
		t.Errorf("unexpected concat ref: %d", dec.ConcatRef)
	}
}

// TestDecodeSMSPDU_Timestamp checks that the SCTS is decoded correctly.
func TestDecodeSMSPDU_Timestamp(t *testing.T) {
	pdu := "00040C9144770000001000006240512100000002C834"
	dec, err := DecodeSMSPDU(pdu)
	if err != nil {
		t.Fatalf("DecodeSMSPDU: %v", err)
	}
	// SCTS bytes: 62 40 51 21 00 00 00
	// YY=26, MM=04, DD=15, hh=12, mm=00, ss=00, tz=+0000
	want := "2026/04/15 12:00:00+0000"
	if dec.Timestamp != want {
		t.Errorf("Timestamp: got %q, want %q", dec.Timestamp, want)
	}
}

// TestDecodeSMSPDU_InvalidHex checks that malformed input returns an error.
func TestDecodeSMSPDU_InvalidHex(t *testing.T) {
	_, err := DecodeSMSPDU("ZZZZ")
	if err == nil {
		t.Fatal("expected error for invalid hex, got nil")
	}
}

// TestParseCMGLPDU checks that parseCMGLPDU extracts messages from PDU-mode
// AT+CMGL=4 output.
func TestParseCMGLPDU_SingleMessage(t *testing.T) {
	// single-part "Hi" from +447700000001
	input := "+CMGL: 1,1,,22\n" +
		"00040C9144770000001000006240512100000002C834\n" +
		"OK\n"
	msgs, err := parseCMGLPDU(input, "SM")
	if err != nil {
		t.Fatalf("parseCMGLPDU: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Sender != "+447700000001" {
		t.Errorf("Sender: got %q", msgs[0].Sender)
	}
	if msgs[0].Text != "Hi" {
		t.Errorf("Text: got %q", msgs[0].Text)
	}
	if msgs[0].Index != 1 {
		t.Errorf("Index: got %d, want 1", msgs[0].Index)
	}
	if msgs[0].Status != "REC READ" {
		t.Errorf("Status: got %q, want %q", msgs[0].Status, "REC READ")
	}
}

// TestParseCMGLPDU_MultiPartExtractsConcat verifies that the concat fields
// are propagated from the UDH into the SMS struct.
func TestParseCMGLPDU_MultiPartExtractsConcat(t *testing.T) {
	input := "+CMGL: 3,0,,29\n" +
		"00440C914477000000100000624051210000000A0500032A020182C221\n" +
		"OK\n"
	msgs, err := parseCMGLPDU(input, "SM")
	if err != nil {
		t.Fatalf("parseCMGLPDU: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.ConcatRef != 42 || m.ConcatTotal != 2 || m.ConcatPart != 1 {
		t.Errorf("concat fields: ref=%d total=%d part=%d (want 42/2/1)",
			m.ConcatRef, m.ConcatTotal, m.ConcatPart)
	}
	if m.Text != "ABC" {
		t.Errorf("Text: got %q, want %q", m.Text, "ABC")
	}
}

// TestParseCMGLPDU_NoMessages checks empty output returns no messages.
func TestParseCMGLPDU_NoMessages(t *testing.T) {
	msgs, err := parseCMGLPDU("OK\n", "SM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}
}

// ── Helper functions ─────────────────────────────────────────────────────

func isHexChar(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
