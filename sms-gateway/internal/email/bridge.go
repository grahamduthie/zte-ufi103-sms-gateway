package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"marlowfm.co.uk/sms-gateway/internal/database"
	"net"
	stdmail "net/mail"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap-idle"
)

type Bridge struct {
	cfg  EmailConfig
	db   *database.DB
	log  Logger
}

type EmailConfig struct {
	IMAPHost       string
	IMAPPort       int
	SMTPHost       string
	SMTPPort       int
	Username       string
	Password       string
	ForwardTo      string
	FromName       string
	AuthorisedSenders []string
}

type Logger interface {
	Printf(format string, v ...interface{})
}

func NewBridge(cfg EmailConfig, db *database.DB, log Logger) *Bridge {
	return &Bridge{cfg: cfg, db: db, log: log}
}

// ForwardMessage sends an SMS as an email to the forwarding address.
func (b *Bridge) ForwardMessage(msg database.Message) error {
	sessionID := b.db.NextDailySequence(1) // UTC+1 for UK time

	subject := fmt.Sprintf("Text from %s [%s]", msg.Sender, sessionID)

	// Parse the received_at time for display
	receivedTime, err := time.Parse(time.RFC3339, msg.ReceivedAt)
	var receivedStr string
	if err != nil {
		receivedStr = msg.ReceivedAt
	} else {
		loc := time.FixedZone("BST", 1*60*60) // UTC+1
		receivedStr = receivedTime.In(loc).Format("02 Jan 2006 15:04:05 MST")
	}

	body := buildHTMLEmail(msg.Body, msg.Sender, receivedStr)

	header := make(map[string]string)
	header["From"] = fmt.Sprintf("%s <%s>", b.cfg.FromName, b.cfg.Username)
	header["To"] = b.cfg.ForwardTo
	header["Subject"] = subject
	header["Reply-To"] = fmt.Sprintf("%s <%s>", b.cfg.FromName, b.cfg.Username)
	header["Date"] = time.Now().Format(time.RFC1123Z)
	header["MIME-Version"] = "1.0"
	header["Content-Type"] = `multipart/mixed; boundary="MSG_BOUNDARY"`

	msgStr := formatMultipartMessage(header, body)

	auth := smtp.PlainAuth("", b.cfg.Username, b.cfg.Password, b.cfg.SMTPHost)

	addr := fmt.Sprintf("%s:%d", b.cfg.SMTPHost, b.cfg.SMTPPort)
	var conn net.Conn
	var errDial error

	if b.cfg.SMTPPort == 465 {
		// Implicit TLS
		conn, errDial = tls.Dial("tcp", addr, &tls.Config{
			ServerName:         b.cfg.SMTPHost,
			InsecureSkipVerify: true,
		})
		if errDial != nil {
			return fmt.Errorf("tls dial: %w", errDial)
		}
	} else {
		// STARTTLS
		conn, errDial = net.DialTimeout("tcp", addr, 15*time.Second)
		if errDial != nil {
			return fmt.Errorf("dial: %w", errDial)
		}
	}
	defer conn.Close()

	c, err := smtp.NewClient(conn, b.cfg.SMTPHost)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	if b.cfg.SMTPPort != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{
				ServerName:         b.cfg.SMTPHost,
				InsecureSkipVerify: true,
			}); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}

	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	if err := c.Mail(b.cfg.Username); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(b.cfg.ForwardTo); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	_, err = io.WriteString(w, msgStr)
	if err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	err = w.Close()
	if err != nil {
		return fmt.Errorf("close data: %w", err)
	}

	// Record in database
	if err := b.db.CreateEmailSession(sessionID, msg.ID, msg.Sender); err != nil {
		b.log.Printf("Warning: failed to create email session: %v", err)
	}
	if err := b.db.MarkForwarded(msg.ID, sessionID); err != nil {
		b.log.Printf("Warning: failed to mark forwarded: %v", err)
	}

	return c.Quit()
}

// SendDeliveryConfirmation emails Graham when a queued SMS reply is sent or permanently fails.
func (b *Bridge) SendDeliveryConfirmation(toNumber, body string, success bool, ref int, failReason, sessionPrefix string) error {
	var subject, statusIcon, statusText, statusColor string
	if success {
		if sessionPrefix != "" {
			subject = fmt.Sprintf("Re: Text from %s [%s]", toNumber, sessionPrefix)
		} else {
			subject = fmt.Sprintf("SMS delivered to %s", toNumber)
		}
		statusIcon = "✅"
		statusText = "Delivered Successfully"
		statusColor = "#16a34a"
	} else {
		if sessionPrefix != "" {
			subject = fmt.Sprintf("Re: Text from %s [%s]", toNumber, sessionPrefix)
		} else {
			subject = fmt.Sprintf("SMS delivery FAILED to %s", toNumber)
		}
		statusIcon = "❌"
		statusText = "Delivery Failed"
		statusColor = "#dc2626"
	}

	html := buildDeliveryHTML(statusIcon, statusText, toNumber, body, ref, failReason, statusColor)

	header := map[string]string{
		"From":         fmt.Sprintf("%s <%s>", b.cfg.FromName, b.cfg.Username),
		"To":           b.cfg.ForwardTo,
		"Subject":      subject,
		"Date":         time.Now().Format(time.RFC1123Z),
		"MIME-Version": "1.0",
		"Content-Type": `multipart/mixed; boundary="MSG_BOUNDARY"`,
	}
	msgStr := formatMultipartMessage(header, html)

	auth := smtp.PlainAuth("", b.cfg.Username, b.cfg.Password, b.cfg.SMTPHost)
	addr := fmt.Sprintf("%s:%d", b.cfg.SMTPHost, b.cfg.SMTPPort)

	var conn net.Conn
	var err error
	if b.cfg.SMTPPort == 465 {
		conn, err = tls.Dial("tcp", addr, &tls.Config{ServerName: b.cfg.SMTPHost, InsecureSkipVerify: true})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 15*time.Second)
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	c, err := smtp.NewClient(conn, b.cfg.SMTPHost)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	if b.cfg.SMTPPort != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: b.cfg.SMTPHost, InsecureSkipVerify: true}); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.Mail(b.cfg.Username); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(b.cfg.ForwardTo); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err = io.WriteString(w, msgStr); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return c.Quit()
}

// SendAdminEmail sends a plain-text administrative email to an arbitrary recipient
// using the gateway's SMTP credentials. Used for keepalive and balance check notifications.
func (b *Bridge) SendAdminEmail(to, subject, body string) error {
	header := map[string]string{
		"From":         fmt.Sprintf("%s <%s>", b.cfg.FromName, b.cfg.Username),
		"To":           to,
		"Subject":      subject,
		"Date":         time.Now().Format(time.RFC1123Z),
		"MIME-Version": "1.0",
		"Content-Type": "text/plain; charset=UTF-8",
	}
	var msg strings.Builder
	for k, v := range header {
		msg.WriteString(k + ": " + v + "\r\n")
	}
	msg.WriteString("\r\n")
	msg.WriteString(body)
	msgStr := msg.String()

	auth := smtp.PlainAuth("", b.cfg.Username, b.cfg.Password, b.cfg.SMTPHost)
	addr := fmt.Sprintf("%s:%d", b.cfg.SMTPHost, b.cfg.SMTPPort)

	var conn net.Conn
	var err error
	if b.cfg.SMTPPort == 465 {
		conn, err = tls.Dial("tcp", addr, &tls.Config{ServerName: b.cfg.SMTPHost, InsecureSkipVerify: true})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 15*time.Second)
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	c, err := smtp.NewClient(conn, b.cfg.SMTPHost)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	if b.cfg.SMTPPort != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: b.cfg.SMTPHost, InsecureSkipVerify: true}); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.Mail(b.cfg.Username); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err = io.WriteString(w, msgStr); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return c.Quit()
}

// PollReplies checks the IMAP inbox for new email replies and queues them as SMS.
func (b *Bridge) PollReplies() error {
	addr := fmt.Sprintf("%s:%d", b.cfg.IMAPHost, b.cfg.IMAPPort)

	// Use a custom dialer with a connect timeout, then set a read/write deadline
	// on the raw connection before handing it to go-imap. This prevents a hung
	// Ionos server from blocking the goroutine indefinitely.
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	rawConn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		ServerName:         b.cfg.IMAPHost,
		InsecureSkipVerify: true,
	})
	if err != nil {
		return fmt.Errorf("imap dial: %w", err)
	}
	rawConn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := client.New(rawConn)
	if err != nil {
		rawConn.Close()
		return fmt.Errorf("imap client: %w", err)
	}
	defer c.Logout()

	if err := c.Login(b.cfg.Username, b.cfg.Password); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}

	if _, err := c.Select("INBOX", false); err != nil {
		return fmt.Errorf("imap select: %w", err)
	}

	// Delegate to shared fetch logic (used by both poll and IDLE paths).
	return b.fetchAndProcessUnseen(c)
}

// IdleLoop runs a persistent IMAP IDLE session with automatic reconnect.
// It blocks until ctx is cancelled. On each reconnect, it fetches and
// processes any unseen messages before re-entering IDLE.
func (b *Bridge) IdleLoop(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		err := b.runIdleSession(ctx)
		if ctx.Err() != nil {
			return // clean shutdown
		}
		b.log.Printf("IMAP IDLE: reconnecting in %v (err: %v)", backoff, err)
		b.db.SetHealth("imap_status", fmt.Sprintf("idle:reconnecting (%v)", err))
		select {
		case <-time.After(backoff):
			if backoff < 5*time.Minute {
				backoff *= 2
			}
		case <-ctx.Done():
			return
		}
	}
}

// runIdleSession establishes a single IMAP IDLE session. It dials, logs in,
// selects INBOX, fetches any unseen messages, then enters IDLE mode.
// Returns on error, ctx cancellation, or successful IDLE exit.
func (b *Bridge) runIdleSession(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", b.cfg.IMAPHost, b.cfg.IMAPPort)

	// Dial with timeout but NO read deadline on the raw connection — IDLE
	// may block for 25 minutes.
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	rawConn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		ServerName:         b.cfg.IMAPHost,
		InsecureSkipVerify: true,
	})
	if err != nil {
		return fmt.Errorf("imap dial: %w", err)
	}
	// Clear any deadline so IDLE can block indefinitely.
	rawConn.SetDeadline(time.Time{})

	c, err := client.New(rawConn)
	if err != nil {
		rawConn.Close()
		return fmt.Errorf("imap client: %w", err)
	}
	defer c.Logout()

	if err := c.Login(b.cfg.Username, b.cfg.Password); err != nil {
		return fmt.Errorf("imap login: %w", err)
	}

	// Set up updates channel BEFORE select.
	updates := make(chan client.Update, 4)
	c.Updates = updates

	if _, err := c.Select("INBOX", false); err != nil {
		return fmt.Errorf("imap select: %w", err)
	}

	// Process any messages already waiting before entering IDLE.
	if err := b.fetchAndProcessUnseen(c); err != nil {
		return err
	}

	b.log.Printf("IMAP IDLE: entering IDLE mode")
	b.db.SetHealth("imap_status", "idle:ok")

	// IDLE loop: enter IDLE, process updates, re-enter. Only exits on
	// error, server disconnect, or context cancellation.
	for {
		idleClient := idle.NewClient(c)
		idleClient.LogoutTimeout = 25 * time.Minute
		stop := make(chan struct{})
		idleErrCh := make(chan error, 1)
		go func() {
			idleErrCh <- idleClient.Idle(stop)
		}()

		select {
		case update := <-updates:
			if mbu, ok := update.(*client.MailboxUpdate); ok && mbu != nil {
				b.log.Printf("IMAP IDLE: mailbox update — messages=%d, recent=%d, unseen=%d",
					mbu.Mailbox.Messages, mbu.Mailbox.Recent, mbu.Mailbox.Unseen)
				// Exit IDLE, fetch unseen, then re-enter IDLE.
				close(stop)
				<-idleErrCh
				if err := b.fetchAndProcessUnseen(c); err != nil {
					return err
				}
				b.db.SetHealth("imap_status", "idle:ok")
				// Loop back and re-enter IDLE.
				continue
			}
			// Other update types — ignore and stay in IDLE.

		case err := <-idleErrCh:
			if err == nil {
				// Clean IDLE exit (timeout/logout) — reconnect.
				return nil
			}
			return fmt.Errorf("idle: %w", err)

		case <-ctx.Done():
			close(stop)
			<-idleErrCh
			b.log.Printf("IMAP IDLE: context cancelled, exiting")
			return nil
		}
	}
}

// fetchAndProcessUnseen searches for unseen messages in INBOX and processes
// each one. Used by both the IDLE wake-up path and the legacy PollReplies.
func (b *Bridge) fetchAndProcessUnseen(c *client.Client) error {
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	ids, err := c.Search(criteria)
	if err != nil {
		return fmt.Errorf("imap search: %w", err)
	}

	if len(ids) == 0 {
		return nil
	}

	b.log.Printf("IMAP: found %d unseen message(s)", len(ids))

	for _, id := range ids {
		seqset := new(imap.SeqSet)
		seqset.AddNum(id)

		messages := make(chan *imap.Message, 1)
		section := &imap.BodySectionName{Peek: true}
		done := make(chan error, 1)

		go func() {
			done <- c.Fetch(seqset, []imap.FetchItem{section.FetchItem()}, messages)
		}()

		msg := <-messages
		if msg == nil {
			b.log.Printf("IMAP: message %d returned nil", id)
			<-done
			continue
		}

		if err := <-done; err != nil {
			b.log.Printf("IMAP: fetch error for message %d: %v", id, err)
			continue
		}

		if err := b.processReply(msg, c); err != nil {
			b.log.Printf("IMAP: process error for message %d: %v", id, err)
		}
	}

	b.db.SetHealth("last_imap_time", time.Now().UTC().Format(time.RFC3339))
	return nil
}

func (b *Bridge) processReply(imapMsg *imap.Message, c *client.Client) error {
	section := &imap.BodySectionName{Peek: true}
	r := imapMsg.GetBody(section)
	if r == nil {
		b.log.Printf("IMAP: message %d has nil body", imapMsg.SeqNum)
		return nil
	}

	// Read the raw email bytes
	rawBody, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	rawStr := string(rawBody)

	// Parse headers for subject/from
	email, err := stdmail.ReadMessage(strings.NewReader(rawStr))
	if err != nil {
		return fmt.Errorf("parse headers: %w", err)
	}

	subject := email.Header.Get("Subject")
	b.log.Printf("IMAP: processing subject: %q", subject)

	prefixRe := regexp.MustCompile(`\[SMS\s+([A-Za-z0-9-]{8,15})\]|\[([A-Za-z0-9-]{8,15})\]`)
	matches := prefixRe.FindStringSubmatch(subject)
	if matches == nil {
		b.log.Printf("IMAP: no session prefix found, skipping")
		return nil
	}
	// Extract the matched prefix (either group 1 for "SMS ..." or group 2 for bare)
	raw := matches[1]
	if raw == "" {
		raw = matches[2]
	}
	// Old format (YYYYMMDD-NNN, 12 chars) stored 8-char prefixes.
	// New format (DDMMYY-NNN, 10 chars) stores 6-char prefixes.
	var prefix string
	if len(raw) >= 12 {
		prefix = raw[:8] // old emails: YYYYMMDD
	} else {
		prefix = raw[:6] // new emails: DDMMYY
	}
	b.log.Printf("IMAP: found session prefix %q in subject (raw=%q)", prefix, raw)

	fromAddr := email.Header.Get("From")
	if !b.isAuthorisedSender(fromAddr) {
		b.log.Printf("IMAP: unauthorised reply from %s, skipping", fromAddr)
		c.Store(imapSeqSet(imapMsg.SeqNum), imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil)
		return nil
	}

	sender, err := b.db.LookupSenderByPrefix(prefix)
	if err != nil {
		b.log.Printf("IMAP: session prefix %q not found in database", prefix)
		c.Store(imapSeqSet(imapMsg.SeqNum), imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil)
		return nil
	}
	b.log.Printf("IMAP: matched sender %s for prefix %s", sender, prefix)

	// Extract plain text from the raw MIME, handling multipart properly.
	// Find the text/plain part by looking for Content-Type: text/plain
	// and extracting everything after the blank line separator.
	body := extractPlainFromBody(rawStr)
	if strings.TrimSpace(body) == "" {
		b.log.Printf("IMAP: empty body, skipping")
		c.Store(imapSeqSet(imapMsg.SeqNum), imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil)
		return nil
	}

	maxChars := 160
	if len(body) > maxChars {
		trunc := body[:maxChars]
		if idx := strings.LastIndex(trunc, " "); idx > maxChars/2 {
			trunc = trunc[:idx]
		}
		body = strings.TrimSpace(trunc)
	}

	_, err = b.db.EnqueueSMS(sender, body, "email_reply", raw)
	if err != nil {
		return fmt.Errorf("enqueue sms: %w", err)
	}

	b.log.Printf("Queued SMS reply to %s: %q", sender, body)

	return c.Store(imapSeqSet(imapMsg.SeqNum), imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil)
}

// extractPlainFromBody finds the text/plain part in a raw MIME email body.
// It looks for Content-Type: text/plain and extracts the body after the
// blank line separator.
func extractPlainFromBody(raw string) string {
	// Find the text/plain boundary
	idx := strings.Index(raw, "Content-Type: text/plain")
	if idx == -1 {
		// No text/plain part found — try raw body cleanup
		return cleanReplyBody(raw)
	}

	// Find the blank line after the Content-Type header
	bodyStart := strings.Index(raw[idx:], "\r\n\r\n")
	if bodyStart == -1 {
		bodyStart = strings.Index(raw[idx:], "\n\n")
		if bodyStart == -1 {
			return cleanReplyBody(raw[idx:])
		}
		bodyStart = idx + bodyStart + 2
	} else {
		bodyStart = idx + bodyStart + 4
	}

	// Find the end of this part (next MIME boundary or end)
	bodyEnd := len(raw)
	nextBoundary := strings.Index(raw[bodyStart:], "\n--")
	if nextBoundary == -1 {
		nextBoundary = strings.Index(raw[bodyStart:], "\r\n--")
	}
	if nextBoundary != -1 {
		bodyEnd = bodyStart + nextBoundary
	}

	body := raw[bodyStart:bodyEnd]
	return cleanReplyBody(body)
}

func (b *Bridge) isAuthorisedSender(fromAddr string) bool {
	// Parse the email address from the From header
	addr, err := stdmail.ParseAddress(fromAddr)
	if err != nil {
		// Try raw comparison
		for _, a := range b.cfg.AuthorisedSenders {
			if strings.EqualFold(fromAddr, a) {
				return true
			}
		}
		return false
	}
	for _, a := range b.cfg.AuthorisedSenders {
		if strings.EqualFold(addr.Address, a) {
			return true
		}
	}
	return false
}

func imapSeqSet(seqNum uint32) *imap.SeqSet {
	s := new(imap.SeqSet)
	s.AddNum(seqNum)
	return s
}

// formatMessage creates a RFC 822 formatted email string.
// Deprecated: use formatMultipartMessage for HTML emails with embedded images.
func formatMessage(headers map[string]string, body string) string {
	var sb strings.Builder
	for key, val := range headers {
		sb.WriteString(fmt.Sprintf("%s: %s\r\n", key, val))
	}
	sb.WriteString("\r\n")
	sb.WriteString(body)
	sb.WriteString("\r\n")
	return sb.String()
}

// logoBase64 holds the Marlow FM logo as base64-encoded PNG data.
var logoBase64 string

// SetLogoBase64 sets the base64-encoded PNG data for the Marlow FM logo.
// Call this once at startup to enable logo embedding in emails.
func SetLogoBase64(b64 string) {
	logoBase64 = b64
}

// buildHTMLEmail constructs an HTML email with the Marlow FM logo,
// the SMS message body, sender phone number, and received timestamp.
func buildHTMLEmail(message, sender, receivedTime string) string {
	// Escape HTML special characters in the message body
	escaped := strings.ReplaceAll(message, "&", "&amp;")
	escaped = strings.ReplaceAll(escaped, "<", "&lt;")
	escaped = strings.ReplaceAll(escaped, ">", "&gt;")
	// Preserve line breaks
	escaped = strings.ReplaceAll(escaped, "\r\n", "<br>\r\n")
	escaped = strings.ReplaceAll(escaped, "\n", "<br>\r\n")

	logoTag := `<img src="cid:logo-image" alt="Marlow FM" style="display:block; width:48px; height:48px; border:0;">`
	if logoBase64 == "" {
		logoTag = ""
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
</head>
<body style="margin:0; padding:0; background:#f4f4f4; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;">
<div style="width:100%%; table-layout:fixed; background:#f4f4f4; padding:20px 0;">
  <div style="max-width:500px; margin:0 auto; background:#ffffff; border-radius:8px; overflow:hidden; box-shadow:0 2px 8px rgba(0,0,0,0.1);">
    <!-- Header -->
    <table style="width:100%%; border-collapse:collapse;">
      <tr>
        <td style="padding:16px 20px; background:#1a1a2e;">
          <table style="border-collapse:collapse;" cellpadding="0" cellspacing="0">
            <tr>
              <td style="padding-right:12px; vertical-align:middle;">
                %s
              </td>
              <td style="vertical-align:middle;">
                <p style="color:#ffffff; font-size:18px; font-weight:600; margin:0;">Marlow FM SMS</p>
                <p style="color:#a0a0b8; font-size:12px; margin:0;">New Message Received</p>
              </td>
            </tr>
          </table>
        </td>
      </tr>
    </table>
    <!-- Content -->
    <div style="padding:24px 20px;">
      <table style="width:100%%; border-collapse:collapse; margin-bottom:16px;" cellpadding="0" cellspacing="0">
        <tr>
          <td style="color:#6b7280; font-size:12px; font-weight:600; text-transform:uppercase; padding:4px 0; width:100px;">From</td>
          <td style="color:#1a1a2e; font-size:14px; padding:4px 0;">%s</td>
        </tr>
        <tr>
          <td style="color:#6b7280; font-size:12px; font-weight:600; text-transform:uppercase; padding:4px 0;">Received</td>
          <td style="color:#1a1a2e; font-size:14px; padding:4px 0;">%s</td>
        </tr>
      </table>
      <div style="background:#f8f8fc; border-left:3px solid #1a1a2e; padding:16px; border-radius:4px;">
        <p style="font-size:16px; line-height:1.5; color:#1a1a2e; margin:0; white-space:pre-wrap;">%s</p>
      </div>
    </div>
  </div>
</div>
</body>
</html>`, logoTag, htmlEscape(sender), receivedTime, escaped)

	return html
}

// buildDeliveryHTML constructs an HTML email for delivery confirmation notices.
func buildDeliveryHTML(statusIcon, statusText, toNumber, body string, ref int, failReason, statusColor string) string {
	extraRow := ""
	if !strings.Contains(statusText, "Successfully") {
		extraRow = fmt.Sprintf(`
        <tr>
          <td style="color:#6b7280; font-size:12px; font-weight:600; text-transform:uppercase; padding:4px 0;">Reason</td>
          <td style="color:#dc2626; font-size:14px; padding:4px 0;">%s</td>
        </tr>`, htmlEscape(failReason))
	}

	logoTag := ""
	if logoBase64 != "" {
		logoTag = `<img src="cid:logo-image" alt="Marlow FM" style="display:block; width:48px; height:48px; border:0;">`
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
</head>
<body style="margin:0; padding:0; background:#f4f4f4; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;">
<div style="width:100%%; table-layout:fixed; background:#f4f4f4; padding:20px 0;">
  <div style="max-width:500px; margin:0 auto; background:#ffffff; border-radius:8px; overflow:hidden; box-shadow:0 2px 8px rgba(0,0,0,0.1);">
    <!-- Header -->
    <table style="width:100%%; border-collapse:collapse;">
      <tr>
        <td style="padding:16px 20px; background:#1a1a2e;">
          <table style="border-collapse:collapse;" cellpadding="0" cellspacing="0">
            <tr>
              <td style="padding-right:12px; vertical-align:middle;">
                %s
              </td>
              <td style="vertical-align:middle;">
                <p style="color:#ffffff; font-size:18px; font-weight:600; margin:0;">Marlow FM SMS</p>
                <p style="color:#a0a0b8; font-size:12px; margin:0;">Delivery Notification</p>
              </td>
            </tr>
          </table>
        </td>
      </tr>
    </table>
    <!-- Content -->
    <div style="padding:24px 20px;">
      <div style="text-align:center; padding:20px; background:#f8f8fc; border-radius:8px; margin:16px 0;">
        <p style="font-size:48px; margin:0;">%s</p>
        <p style="font-size:18px; font-weight:600; color:%s; margin:8px 0 0;">%s</p>
      </div>
      <table style="width:100%%; border-collapse:collapse; margin-top:16px;" cellpadding="0" cellspacing="0">
        <tr>
          <td style="color:#6b7280; font-size:12px; font-weight:600; text-transform:uppercase; padding:4px 0; width:100px;">To</td>
          <td style="color:#1a1a2e; font-size:14px; padding:4px 0;">%s</td>
        </tr>
        <tr>
          <td style="color:#6b7280; font-size:12px; font-weight:600; text-transform:uppercase; padding:4px 0;">Message</td>
          <td style="color:#1a1a2e; font-size:14px; padding:4px 0;">%s</td>
        </tr>%s
      </table>
    </div>
    <!-- Footer -->
    <div style="background:#f8f8fc; padding:16px 20px; text-align:center;">
      <p style="color:#6b7280; font-size:12px; line-height:1.6; margin:0;">Marlow FM SMS Gateway — Delivery Notification</p>
    </div>
  </div>
</div>
</body>
</html>`, logoTag, statusIcon, statusColor, statusText, toNumber, htmlEscape(body), extraRow)

	return html
}

// htmlEscape escapes special characters for safe inclusion in HTML content.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

// formatMultipartMessage builds a MIME multipart email with an HTML body and
// an embedded logo image (as a CID attachment).
//
// Uses multipart/related for the HTML+logo pair (RFC 2387), nested inside
// multipart/mixed. This ensures clients like Thunderbird resolve cid:
// references inline instead of showing the image as an attachment.
func formatMultipartMessage(headers map[string]string, htmlBody string) string {
	var sb strings.Builder

	// Write headers
	for key, val := range headers {
		sb.WriteString(fmt.Sprintf("%s: %s\r\n", key, val))
	}
	sb.WriteString("\r\n")

	// Outer multipart/mixed boundary
	sb.WriteString("--MSG_BOUNDARY\r\n")

	// Inner multipart/related — tells the client that the HTML and logo
	// belong together; cid: references are resolved inline.
	sb.WriteString("Content-Type: multipart/related; boundary=\"RELATED_BOUNDARY\"\r\n")
	sb.WriteString("\r\n")

	// HTML part
	sb.WriteString("--RELATED_BOUNDARY\r\n")
	sb.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	sb.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(toQuotedPrintable(htmlBody))
	sb.WriteString("\r\n\r\n")

	// Logo part (if available)
	if logoBase64 != "" {
		sb.WriteString("--RELATED_BOUNDARY\r\n")
		sb.WriteString("Content-Type: image/png; name=\"logo.png\"\r\n")
		sb.WriteString("Content-Transfer-Encoding: base64\r\n")
		sb.WriteString("Content-ID: <logo-image>\r\n")
		sb.WriteString("Content-Disposition: inline; filename=\"logo.png\"\r\n")
		sb.WriteString("\r\n")
		// Wrap base64 at 76 chars per RFC 2045
		data := logoBase64
		for len(data) > 76 {
			sb.WriteString(data[:76])
			sb.WriteString("\r\n")
			data = data[76:]
		}
		if len(data) > 0 {
			sb.WriteString(data)
			sb.WriteString("\r\n")
		}
	}

	sb.WriteString("--RELATED_BOUNDARY--\r\n")
	sb.WriteString("--MSG_BOUNDARY--\r\n")
	return sb.String()
}

// toQuotedPrintable encodes a string as quoted-printable per RFC 2045.
func toQuotedPrintable(s string) string {
	var sb strings.Builder
	lineLen := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '\n' {
			sb.WriteString("\r\n")
			lineLen = 0
			continue
		}
		if b == ' ' || b == '\t' || (b >= 33 && b <= 60) || (b >= 62 && b <= 126) {
			if lineLen >= 73 {
				sb.WriteString("=\r\n")
				lineLen = 0
			}
			sb.WriteByte(b)
			lineLen++
		} else {
			if lineLen >= 73 {
				sb.WriteString("=\r\n")
				lineLen = 0
			}
			sb.WriteString(fmt.Sprintf("=%02X", b))
			lineLen += 3
		}
	}
	return sb.String()
}

// cleanReplyBody strips quoted reply text, signatures, and forwarding
// footers from an email body to extract the user's actual reply text.
// It also decodes quoted-printable encoding and normalises non-GSM
// characters (smart quotes, em-dashes, etc.) to ASCII equivalents so
// the SMS modem can handle them.
//
// Handles these common client formats:
// - Gmail:     "On Sat, 4 Apr 2026 at 13:41, Someone wrote:"
// - Outlook:   "From: Someone <someone@email.com>\nSent: 04 April 2026..."
// - Apple:     "On 4 Apr 2026, at 13:41, Someone wrote:"
// - Thunderbird: "On 04/04/2026 13:41, Someone wrote:"
// - Plain:     "> quoted text"
func cleanReplyBody(text string) string {
	// Decode quoted-printable encoding (=E2=80=99 etc.)
	if strings.Contains(text, "=") {
		text = decodeQuotedPrintable(text)
	}

	// Normalise non-GSM characters to ASCII equivalents
	text = normaliseToGSM(text)

	// Strip everything from the separator line onwards
	if idx := strings.Index(text, "\n---"); idx != -1 {
		text = text[:idx]
	}

	// Strip Gmail/Apple/Thunderbird "On ... wrote:" blocks
	// Matches: "On <anything>, ... wrote:" or "On <date> at <time>, ... wrote:"
	onWroteRe := regexp.MustCompile(`(?m)^On\s+.+,\s*.+wrote:.*$`)
	if loc := onWroteRe.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}

	// Strip Outlook-style headers: "From: ...\nSent: ...\nTo: ...\nSubject: ..."
	if loc := regexp.MustCompile(`(?m)^From:`).FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}

	// Strip any lines starting with > (quoted reply)
	var result []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		result = append(result, line)
	}
	text = strings.Join(result, "\n")

	return strings.TrimSpace(text)
}

// decodeQuotedPrintable decodes quoted-printable encoded text (=XX hex escapes
// and soft line breaks =\r\n).
func decodeQuotedPrintable(s string) string {
	// Remove soft line breaks (= followed by \r\n or \n)
	s = regexp.MustCompile(`=\r?\n`).ReplaceAllString(s, "")
	// Decode =XX hex escapes — accumulate bytes, then interpret as UTF-8.
	re := regexp.MustCompile(`=([0-9A-Fa-f]{2})`)
	var buf []byte
	lastEnd := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		buf = append(buf, []byte(s[lastEnd:m[0]])...)
		hex := s[m[2]:m[3]]
		b, err := strconv.ParseUint(hex, 16, 8)
		if err != nil {
			buf = append(buf, []byte(s[m[0]:m[1]])...)
		} else {
			buf = append(buf, byte(b))
		}
		lastEnd = m[1]
	}
	buf = append(buf, []byte(s[lastEnd:])...)
	return string(buf)
}

// normaliseToGSM replaces characters that the GSM 7-bit charset can't encode
// with their closest ASCII equivalents.
func normaliseToGSM(s string) string {
	// Common Unicode → ASCII substitutions
	replacements := []struct{ from, to string }{
		{"\u2018", "'"}, // ' LEFT SINGLE QUOTATION MARK
		{"\u2019", "'"}, // ' RIGHT SINGLE QUOTATION MARK
		{"\u201C", "\""}, // " LEFT DOUBLE QUOTATION MARK
		{"\u201D", "\""}, // " RIGHT DOUBLE QUOTATION MARK
		{"\u2013", "-"}, // – EN DASH
		{"\u2014", "-"}, // — EM DASH
		{"\u2026", "..."}, // … HORIZONTAL ELLIPSIS
		{"\u00A0", " "}, //  NO-BREAK SPACE
		{"\u200B", ""},  // ​ ZERO WIDTH SPACE
	}
	for _, r := range replacements {
		s = strings.ReplaceAll(s, r.from, r.to)
	}
	// Strip any remaining non-ASCII characters
	var out strings.Builder
	for _, r := range s {
		if r >= 32 && r <= 126 {
			out.WriteRune(r)
		} else if r == '\n' || r == '\r' || r == '\t' {
			out.WriteRune(r)
		}
		// else silently drop the character
	}
	return out.String()
}
