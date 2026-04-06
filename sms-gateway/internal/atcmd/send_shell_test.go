package atcmd

import (
	"strings"
	"testing"
)

// ── sendSMSViaShell injection tests ──────────────────────────────────────
// These tests verify that the shell injection prevention works correctly.
// They do NOT actually execute shell commands — they test the validation
// logic that prevents injection.

func TestSendSMSViaShell_Injection_Backtick(t *testing.T) {
	_, err := sendSMSViaShell("`rm -rf /`", "test")
	if err == nil {
		t.Fatal("expected error for backtick injection in number, got nil")
	}
}

func TestSendSMSViaShell_Injection_Semicolon(t *testing.T) {
	_, err := sendSMSViaShell("+447734; rm -rf /", "test")
	if err == nil {
		t.Fatal("expected error for semicolon injection in number, got nil")
	}
}

func TestSendSMSViaShell_Injection_Pipe(t *testing.T) {
	_, err := sendSMSViaShell("+447734 | cat /etc/passwd", "test")
	if err == nil {
		t.Fatal("expected error for pipe injection in number, got nil")
	}
}

func TestSendSMSViaShell_Injection_Ampersand(t *testing.T) {
	_, err := sendSMSViaShell("+447734 & whoami", "test")
	if err == nil {
		t.Fatal("expected error for ampersand injection in number, got nil")
	}
}

func TestSendSMSViaShell_Injection_DollarParen(t *testing.T) {
	_, err := sendSMSViaShell("$(cat /etc/passwd)", "test")
	if err == nil {
		t.Fatal("expected error for $(...) injection in number, got nil")
	}
}

func TestSendSMSViaShell_Injection_TextBacktick(t *testing.T) {
	_, err := sendSMSViaShell("+447700000001", "`rm -rf /`")
	if err == nil {
		t.Fatal("expected error for backtick injection in text, got nil")
	}
}

func TestSendSMSViaShell_Injection_TextSemicolon(t *testing.T) {
	_, err := sendSMSViaShell("+447700000001", "hello; rm -rf /")
	if err == nil {
		t.Fatal("expected error for semicolon injection in text, got nil")
	}
}

func TestSendSMSViaShell_Injection_TextPipe(t *testing.T) {
	_, err := sendSMSViaShell("+447700000001", "hello | cat /etc/passwd")
	if err == nil {
		t.Fatal("expected error for pipe injection in text, got nil")
	}
}

func TestSendSMSViaShell_Injection_TextDollar(t *testing.T) {
	_, err := sendSMSViaShell("+447700000001", "$(whoami)")
	if err == nil {
		t.Fatal("expected error for $(...) injection in text, got nil")
	}
}

func TestSendSMSViaShell_Injection_TextBackslash(t *testing.T) {
	_, err := sendSMSViaShell("+447700000001", "hello\\world")
	if err == nil {
		t.Fatal("expected error for backslash injection in text, got nil")
	}
}

func TestSendSMSViaShell_ValidNumber(t *testing.T) {
	// These should pass validation (but fail at actual send since we can't
	// open /dev/smd11 in tests)
	validNumbers := []string{
		"+447700000001",
		"447700000001",
		"+12125551234",
		"+44 7734 139947",
		"+44-7734-139947",
		"+44(7734)139947",
	}
	for _, num := range validNumbers {
		_, err := sendSMSViaShell(num, "test")
		// We expect an error (can't open /dev/smd11 in test), but NOT
		// a validation error
		if err != nil && strings.Contains(err.Error(), "invalid phone number") {
			t.Errorf("valid number %q was rejected: %v", num, err)
		}
	}
}

func TestSendSMSViaShell_ValidText(t *testing.T) {
	// These should pass validation
	validTexts := []string{
		"Hello world",
		"Test 123",
		"Hello! How are you?",
		"It's a test",      // single quotes are escaped
		"Price: £50",       // pound sign (non-GSM but not shell injection)
		"Café résumé",      // unicode (not shell injection)
	}
	for _, text := range validTexts {
		_, err := sendSMSViaShell("+447700000001", text)
		// We expect an error (can't open /dev/smd11), but NOT a validation error
		if err != nil && strings.Contains(err.Error(), "contains characters that are not permitted") {
			t.Errorf("valid text %q was rejected: %v", text, err)
		}
	}
}

func TestSendSMSViaShell_EmptyNumber(t *testing.T) {
	_, err := sendSMSViaShell("", "test")
	if err == nil {
		t.Fatal("expected error for empty number, got nil")
	}
	if !strings.Contains(err.Error(), "invalid phone number") {
		t.Fatalf("expected validation error, got: %v", err)
	}
}
