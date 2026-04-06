package email

import (
	"regexp"
	"strings"
	"testing"
)

// ── cleanReplyBody tests ──────────────────────────────────────────────────

func TestCleanReplyBody_PlainText(t *testing.T) {
	body := "Hello, this is a test message."
	result := cleanReplyBody(body)
	if result != body {
		t.Fatalf("expected %q, got %q", body, result)
	}
}

func TestCleanReplyBody_GmailQuote(t *testing.T) {
	body := `Thanks for the update!

On Sat, 4 Apr 2026 at 13:41, Graham Duthie wrote:
> Original message here`
	result := cleanReplyBody(body)
	expected := "Thanks for the update!"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_OutlookQuote(t *testing.T) {
	body := `Sure thing!

From: Graham Duthie <graham@example.com>
Sent: 04 April 2026 14:00
To: User <user@example.com>
Subject: Re: Test

Original message`
	result := cleanReplyBody(body)
	expected := "Sure thing!"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_AppleQuote(t *testing.T) {
	body := `Looks good!

On 4 Apr 2026, at 13:41, Graham wrote:
> Previous message`
	result := cleanReplyBody(body)
	expected := "Looks good!"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_ThunderbirdQuote(t *testing.T) {
	body := `OK.

On 04/04/2026 13:41, Graham wrote:
> Previous`
	result := cleanReplyBody(body)
	expected := "OK."
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_QuotedLines(t *testing.T) {
	body := `Here's my reply.
> This is quoted.
> So is this.
> > And this is double-quoted.

Extra text.`
	result := cleanReplyBody(body)
	expected := "Here's my reply.\n\nExtra text."
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_Separator(t *testing.T) {
	body := `My reply.

---
Signature`
	result := cleanReplyBody(body)
	expected := "My reply."
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_QuotedPrintable(t *testing.T) {
	body := "Here=E2=80=99s a test"
	result := cleanReplyBody(body)
	expected := "Here's a test"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_NormaliseGSM(t *testing.T) {
	body := "It's a \u201Ctest\u201D with an \u2014 em-dash and\u2026"
	result := cleanReplyBody(body)
	expected := "It's a \"test\" with an - em-dash and..."
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_UnicodeStripped(t *testing.T) {
	body := "Hello\u00A0World\u200BTest"
	result := cleanReplyBody(body)
	expected := "Hello WorldTest"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestCleanReplyBody_Empty(t *testing.T) {
	result := cleanReplyBody("")
	if result != "" {
		t.Fatalf("expected empty string, got %q", result)
	}
}

func TestCleanReplyBody_OnlyWhitespace(t *testing.T) {
	result := cleanReplyBody("   \n\n  ")
	if result != "" {
		t.Fatalf("expected empty string, got %q", result)
	}
}

func TestCleanReplyBody_NoTruncationWithoutSeparator(t *testing.T) {
	body := `Line 1
Line 2
Line 3`
	result := cleanReplyBody(body)
	if result != body {
		t.Fatalf("expected %q, got %q", body, result)
	}
}

func TestCleanReplyBody_SoftLineBreaks(t *testing.T) {
	body := "This is a=\r\nlong line"
	result := cleanReplyBody(body)
	expected := "This is along line"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

// ── normaliseToGSM tests ──────────────────────────────────────────────────

func TestNormaliseToGSM_LeftSingleQuote(t *testing.T) {
	result := normaliseToGSM("\u2018hello\u2019")
	if result != "'hello'" {
		t.Fatalf("expected %q, got %q", "'hello'", result)
	}
}

func TestNormaliseToGSM_RightSingleQuote(t *testing.T) {
	result := normaliseToGSM("it\u2019s")
	if result != "it's" {
		t.Fatalf("expected %q, got %q", "it's", result)
	}
}

func TestNormaliseToGSM_LeftDoubleQuote(t *testing.T) {
	result := normaliseToGSM("\u201Chello\u201D")
	if result != "\"hello\"" {
		t.Fatalf("expected %q, got %q", "\"hello\"", result)
	}
}

func TestNormaliseToGSM_RightDoubleQuote(t *testing.T) {
	result := normaliseToGSM("she said \u201Chi\u201D")
	if result != "she said \"hi\"" {
		t.Fatalf("expected %q, got %q", "she said \"hi\"", result)
	}
}

func TestNormaliseToGSM_EnDash(t *testing.T) {
	result := normaliseToGSM("pages 1\u201310")
	if result != "pages 1-10" {
		t.Fatalf("expected %q, got %q", "pages 1-10", result)
	}
}

func TestNormaliseToGSM_EmDash(t *testing.T) {
	result := normaliseToGSM("word1\u2014word2")
	if result != "word1-word2" {
		t.Fatalf("expected %q, got %q", "word1-word2", result)
	}
}

func TestNormaliseToGSM_Ellipsis(t *testing.T) {
	result := normaliseToGSM("wait...")
	if result != "wait..." {
		t.Fatalf("expected %q, got %q", "wait...", result)
	}
}

func TestNormaliseToGSM_NonASCIIStripped(t *testing.T) {
	result := normaliseToGSM("Hello\u00E9World") // é is non-ASCII
	if result != "HelloWorld" {
		t.Fatalf("expected %q, got %q", "HelloWorld", result)
	}
}

func TestNormaliseToGSM_PureASCII(t *testing.T) {
	input := "Hello, world! 123"
	result := normaliseToGSM(input)
	if result != input {
		t.Fatalf("expected %q, got %q", input, result)
	}
}

func TestNormaliseToGSM_NonBreakingSpace(t *testing.T) {
	result := normaliseToGSM("Hello\u00A0World")
	if result != "Hello World" {
		t.Fatalf("expected %q, got %q", "Hello World", result)
	}
}

// ── decodeQuotedPrintable tests ───────────────────────────────────────────

func TestDecodeQuotedPrintable_Simple(t *testing.T) {
	input := "Hello=20World"
	result := decodeQuotedPrintable(input)
	if result != "Hello World" {
		t.Fatalf("expected %q, got %q", "Hello World", result)
	}
}

func TestDecodeQuotedPrintable_SmartQuote(t *testing.T) {
	input := "Here=E2=80=99s"
	result := decodeQuotedPrintable(input)
	expected := "Here’s" // U+2019 encoded as UTF-8: E2 80 99
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestDecodeQuotedPrintable_SoftLineBreak(t *testing.T) {
	input := "This is=\r\na long line"
	result := decodeQuotedPrintable(input)
	// Soft line break removes the =\r\n but doesn't insert a space
	expected := "This isa long line"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestDecodeQuotedPrintable_NoEncoding(t *testing.T) {
	input := "Plain text, no encoding"
	result := decodeQuotedPrintable(input)
	if result != input {
		t.Fatalf("expected %q, got %q", input, result)
	}
}

func TestDecodeQuotedPrintable_MultipleHex(t *testing.T) {
	input := "=E2=80=9CHello=E2=80=9D"
	result := decodeQuotedPrintable(input)
	expected := "\u201CHello\u201D"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestDecodeQuotedPrintable_LowerCaseHex(t *testing.T) {
	input := "=e2=80=99"
	result := decodeQuotedPrintable(input)
	expected := "\u2019" // U+2019
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

// ── isAuthorisedSender tests ──────────────────────────────────────────────

func TestIsAuthorisedSender_ExactMatch(t *testing.T) {
	b := &Bridge{
		cfg: EmailConfig{
			AuthorisedSenders: []string{"graham@example.com"},
		},
	}
	if !b.isAuthorisedSender("graham@example.com") {
		t.Fatal("expected authorised")
	}
}

func TestIsAuthorisedSender_CaseInsensitive(t *testing.T) {
	b := &Bridge{
		cfg: EmailConfig{
			AuthorisedSenders: []string{"graham@example.com"},
		},
	}
	if !b.isAuthorisedSender("Graham@Example.COM") {
		t.Fatal("expected case-insensitive match to be authorised")
	}
}

func TestIsAuthorisedSender_NotMatch(t *testing.T) {
	b := &Bridge{
		cfg: EmailConfig{
			AuthorisedSenders: []string{"graham@example.com"},
		},
	}
	if b.isAuthorisedSender("other@example.com") {
		t.Fatal("expected unauthorised")
	}
}

func TestIsAuthorisedSender_FromHeader(t *testing.T) {
	b := &Bridge{
		cfg: EmailConfig{
			AuthorisedSenders: []string{"graham@example.com"},
		},
	}
	if !b.isAuthorisedSender("Graham <graham@example.com>") {
		t.Fatal("expected From header with name to be parsed and authorised")
	}
}

func TestIsAuthorisedSender_EmptyList(t *testing.T) {
	b := &Bridge{
		cfg: EmailConfig{
			AuthorisedSenders: []string{},
		},
	}
	if b.isAuthorisedSender("any@example.com") {
		t.Fatal("expected unauthorised with empty list")
	}
}

func TestIsAuthorisedSender_MalformedFrom(t *testing.T) {
	b := &Bridge{
		cfg: EmailConfig{
			AuthorisedSenders: []string{"graham@example.com"},
		},
	}
	// Malformed address falls back to raw comparison
	if b.isAuthorisedSender("not an email address") {
		t.Fatal("expected malformed address to be unauthorised")
	}
}

// ── HTML email tests ─────────────────────────────────────────────────────

func TestBuildHTMLEmail_ContainsMessage(t *testing.T) {
	html := buildHTMLEmail("Hello World", "+447700000001", "06 Apr 2026 10:30:00 BST", "060426-001")
	if !strings.Contains(html, "Hello World") {
		t.Fatal("expected message body in HTML")
	}
}

func TestBuildHTMLEmail_ContainsSender(t *testing.T) {
	html := buildHTMLEmail("Test", "+447700000001", "06 Apr 2026 10:30:00 BST", "060426-001")
	if !strings.Contains(html, "+447700000001") {
		t.Fatal("expected sender number in HTML")
	}
}

func TestBuildHTMLEmail_ContainsReceivedTime(t *testing.T) {
	html := buildHTMLEmail("Test", "+447700000001", "06 Apr 2026 10:30:00 BST", "060426-001")
	if !strings.Contains(html, "06 Apr 2026 10:30:00 BST") {
		t.Fatal("expected received time in HTML")
	}
}

func TestBuildHTMLEmail_ContainsSessionID(t *testing.T) {
	html := buildHTMLEmail("Test", "+447700000001", "06 Apr 2026 10:30:00 BST", "060426-001")
	if !strings.Contains(html, "[060426-001]") {
		t.Fatal("expected session ID in HTML")
	}
}

func TestBuildHTMLEmail_MetaBeforeMessage(t *testing.T) {
	html := buildHTMLEmail("Hello World", "+447700000001", "06 Apr 2026 10:30:00 BST", "060426-001")
	// Meta table (From/Received/Reference) should appear before message body
	fromIdx := strings.Index(html, ">From<")
	bodyIdx := strings.Index(html, ">Hello World<")
	if fromIdx == -1 || bodyIdx == -1 {
		t.Fatalf("could not find expected markers in HTML")
	}
	if bodyIdx < fromIdx {
		t.Fatal("expected meta section before message body")
	}
}

func TestBuildHTMLEmail_NoFooterInstructions(t *testing.T) {
	html := buildHTMLEmail("Test", "+447700000001", "06 Apr 2026", "060426-001")
	if strings.Contains(html, "Reply to this email") || strings.Contains(html, "Keep replies under") {
		t.Fatal("expected no reply instructions in HTML")
	}
}

func TestBuildHTMLEmail_EscapesHTML(t *testing.T) {
	html := buildHTMLEmail("<script>alert('xss')</script>", "+447700000001", "06 Apr 2026", "060426-001")
	if strings.Contains(html, "<script>") {
		t.Fatal("expected HTML escaping in message body")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("expected &lt;script&gt; in HTML")
	}
}

func TestBuildHTMLEmail_PreservesLineBreaks(t *testing.T) {
	html := buildHTMLEmail("Line 1\nLine 2", "+447700000001", "06 Apr 2026", "060426-001")
	if !strings.Contains(html, "<br>") {
		t.Fatal("expected <br> tags for line breaks")
	}
}

func TestBuildHTMLEmail_WithLogo(t *testing.T) {
	SetLogoBase64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
	defer SetLogoBase64("")

	html := buildHTMLEmail("Test", "+447700000001", "06 Apr 2026", "060426-001")
	if !strings.Contains(html, "cid:logo-image") {
		t.Fatal("expected cid:logo-image in HTML when logo is set")
	}
}

func TestBuildHTMLEmail_WithoutLogo(t *testing.T) {
	SetLogoBase64("")
	html := buildHTMLEmail("Test", "+447700000001", "06 Apr 2026", "060426-001")
	if strings.Contains(html, "cid:logo-image") {
		t.Fatal("expected no cid:logo-image when logo is not set")
	}
}

func TestBuildDeliveryHTML_Success(t *testing.T) {
	SetLogoBase64("")
	html := buildDeliveryHTML("✅", "Delivered Successfully", "+447700000001", "Hello", 42, "", "#16a34a")
	if !strings.Contains(html, "Delivered Successfully") {
		t.Fatal("expected success status in HTML")
	}
	if !strings.Contains(html, "+447700000001") {
		t.Fatal("expected recipient number in HTML")
	}
	if !strings.Contains(html, "42") {
		t.Fatal("expected modem ref in HTML")
	}
	if strings.Contains(html, "Reason") {
		t.Fatal("expected no reason row in success HTML")
	}
}

func TestBuildDeliveryHTML_Failure(t *testing.T) {
	SetLogoBase64("")
	html := buildDeliveryHTML("❌", "Delivery Failed", "+447700000001", "Hello", 0, "Network timeout", "#dc2626")
	if !strings.Contains(html, "Delivery Failed") {
		t.Fatal("expected failure status in HTML")
	}
	if !strings.Contains(html, "Network timeout") {
		t.Fatal("expected failure reason in HTML")
	}
}

func TestFormatMultipartMessage_HasMIMEBoundary(t *testing.T) {
	SetLogoBase64("")
	msg := formatMultipartMessage(map[string]string{"Subject": "Test"}, "<p>Hello</p>")
	if !strings.Contains(msg, "--MSG_BOUNDARY") {
		t.Fatal("expected MIME boundary in multipart message")
	}
	if !strings.Contains(msg, "Content-Type: text/html") {
		t.Fatal("expected HTML content type in multipart message")
	}
}

func TestFormatMultipartMessage_IncludesLogo(t *testing.T) {
	SetLogoBase64("iVBORw0KGgo")
	msg := formatMultipartMessage(map[string]string{"Subject": "Test"}, "<p>Hello</p>")
	if !strings.Contains(msg, "Content-ID: <logo-image>") {
		t.Fatal("expected Content-ID for logo in multipart message")
	}
	if !strings.Contains(msg, "Content-Type: image/png") {
		t.Fatal("expected image/png content type for logo")
	}
	SetLogoBase64("")
}

func TestToQuotedPrintable_EncodesSpecialChars(t *testing.T) {
	result := toQuotedPrintable("Hello=World")
	if !strings.Contains(result, "=3D") {
		t.Fatalf("expected =3D for equals sign, got %q", result)
	}
}

func TestToQuotedPrintable_PreservesASCII(t *testing.T) {
	result := toQuotedPrintable("Hello World 123")
	if result != "Hello World 123" {
		t.Fatalf("expected plain ASCII unchanged, got %q", result)
	}
}

func TestHTMLEscape(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"<script>", "&lt;script&gt;"},
		{"A & B", "A &amp; B"},
		{`"quoted"`, "&quot;quoted&quot;"},
		{"plain text", "plain text"},
	}
	for _, tt := range tests {
		result := htmlEscape(tt.input)
		if result != tt.expected {
			t.Fatalf("htmlEscape(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestSessionPrefixRegex(t *testing.T) {
	prefixRe := regexp.MustCompile(`\[SMS\s+([A-Za-z0-9-]{8,15})\]|\[([A-Za-z0-9-]{8,15})\]`)

	tests := []struct {
		subject  string
		expected string
	}{
		// New format: reference at end of subject
		{"Re: Text from +447700000001 [060426-001]", "060426"},
		{"Text from +447700000001 [060426-001]", "060426"},
		{"Re: Text from +441234567890 [060426-042]", "060426"},
		// Old format: [SMS YYYYMMDD-NNN] at start
		{"[SMS 20260406-001] From +447700000001", "20260406"},
		{"Re: [SMS 20260406-003] From +447700000001", "20260406"},
		// Old nanosecond format
		{"[SMS 5-1712412345] From +447700000001", "5-171241"},
	}

	for _, tt := range tests {
		matches := prefixRe.FindStringSubmatch(tt.subject)
		if matches == nil {
			t.Fatalf("regex failed on subject %q", tt.subject)
		}
		raw := matches[1]
		if raw == "" {
			raw = matches[2]
		}
		var prefix string
		if len(raw) >= 12 {
			prefix = raw[:8]
		} else {
			prefix = raw[:6]
		}
		if prefix != tt.expected {
			t.Fatalf("subject %q: got prefix %q (raw=%q), want %q", tt.subject, prefix, raw, tt.expected)
		}
	}
}
