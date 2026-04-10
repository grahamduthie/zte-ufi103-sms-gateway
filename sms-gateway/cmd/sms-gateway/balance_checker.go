package main

// balance_checker.go — weekly GiffGaff balance check via SMS.
//
// Every Sunday at ~10am UK time, sends "INFO" to 85075 (GiffGaff balance
// service). Waits up to 10 minutes for a response SMS from any service sender
// (non-E.164 address). Forwards the response to adminEmail. If no response
// arrives within 10 minutes, sends a warning email.
//
// The INFO SMS uses source='balance_check' so it is never counted as a
// chargeable text for the SIM keepalive tracker.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"marlowfm.co.uk/sms-gateway/internal/database"
	"marlowfm.co.uk/sms-gateway/internal/email"
)

const (
	giffgafNumber          = "85075"
	balanceCheckHour       = 10 // 10am UK time (ukNow = UTC+1)
	balanceResponseTimeout = 10 * time.Minute
)

// adminEmail is set at startup from config (email.admin_email).
// Declared as a package-level var so all goroutines in this package share it
// without needing to thread the value through every function call.
var adminEmail string

// runBalanceChecker ticks every minute, triggers on Sunday at 10am UK time,
// enqueues "INFO" to 85075, and waits up to 10 minutes for a response SMS.
func runBalanceChecker(ctx context.Context, db *database.DB, bridge *email.Bridge, logger *log.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()

	var lastCheckDate string   // "YYYY-MM-DD" — prevents double-firing in the same hour
	var waitDeadline time.Time // non-zero means we're waiting for a response

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		now := time.Now().UTC()
		ukNow := now.Add(time.Hour) // approximate UK time (UTC+1 / BST)

		// Check timeout: no response arrived within the 10-minute window.
		if !waitDeadline.IsZero() && now.After(waitDeadline) {
			logger.Printf("Balance checker: no response from GiffGaff within 10 minutes")
			db.SetHealth("balance_check_pending_since", "")
			waitDeadline = time.Time{}
			if bridge != nil {
				subj := "GiffGaff balance check: no response received"
				body := fmt.Sprintf(
					"A balance check was sent to GiffGaff (85075) but no response arrived within 10 minutes.\n\nCheck date: %s\n",
					lastCheckDate,
				)
				if err := bridge.SendAdminEmail(adminEmail, subj, body); err != nil {
					logger.Printf("Balance checker: timeout warning email failed: %v", err)
				}
			}
		}

		// Trigger: Sunday, 10am UK time, not already run today.
		today := ukNow.Format("2006-01-02")
		if ukNow.Weekday() == time.Sunday && ukNow.Hour() == balanceCheckHour && today != lastCheckDate {
			lastCheckDate = today
			logger.Printf("Balance checker: sending INFO to %s", giffgafNumber)
			_, err := db.EnqueueSMS(giffgafNumber, "INFO", "balance_check", "")
			if err != nil {
				logger.Printf("Balance checker: failed to enqueue INFO: %v", err)
				lastCheckDate = "" // allow retry next minute
				continue
			}
			db.SetHealth("balance_check_pending_since", now.Format(time.RFC3339))
			waitDeadline = now.Add(balanceResponseTimeout)
			logger.Printf("Balance checker: INFO enqueued, waiting up to 10 minutes for response")
		}
	}
}

// handleIncomingBalanceResponse checks whether an incoming SMS is a GiffGaff
// balance response. If so, it forwards the response to adminEmail and marks the
// message as forwarded (suppressing the normal SMS-to-email path). Returns true
// if the message was handled as a balance response.
func handleIncomingBalanceResponse(db *database.DB, bridge *email.Bridge, logger *log.Logger, msgID int64, sender, body string) bool {
	// Balance responses come from service senders (short codes / named), not E.164 numbers.
	if !isServiceSender(sender) {
		return false
	}

	// Only handle responses while a check is pending.
	pendingSince, err := db.GetHealth("balance_check_pending_since")
	if err == sql.ErrNoRows || pendingSince == "" {
		return false
	}
	if err != nil {
		return false
	}

	// Verify we're still within the 10-minute response window.
	sentAt, err := time.Parse(time.RFC3339, pendingSince)
	if err != nil || time.Now().UTC().After(sentAt.Add(balanceResponseTimeout)) {
		return false
	}

	logger.Printf("Balance checker: response received from %q", sender)
	db.SetHealth("balance_check_pending_since", "")

	if bridge != nil {
		subj := fmt.Sprintf("GiffGaff balance check response (from %s)", sender)
		emailBody := fmt.Sprintf("GiffGaff balance response received from %s:\n\n%s\n", sender, body)
		if err := bridge.SendAdminEmail(adminEmail, subj, emailBody); err != nil {
			logger.Printf("Balance checker: failed to send response email: %v", err)
		}
	}

	// Suppress normal forwarding.
	db.MarkForwarded(msgID, "balance-response")
	return true
}

// isServiceSender returns true if the sender looks like a short code or named
// sender (e.g. "giffgaff", "85075") rather than a standard E.164 phone number.
// GiffGaff balance responses come from a named or short-code sender, not a +44… number.
func isServiceSender(sender string) bool {
	return !strings.HasPrefix(sender, "+")
}

// isGiffGafDest returns true if the destination number is the GiffGaff
// balance service (85075) or similar short-code destination.
func isGiffGafDest(toNumber string) bool {
	return toNumber == giffgafNumber
}
