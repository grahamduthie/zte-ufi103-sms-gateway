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

// ── Helper functions ─────────────────────────────────────────────────────

func isHexChar(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
