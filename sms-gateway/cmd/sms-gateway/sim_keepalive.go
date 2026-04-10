package main

// sim_keepalive.go — keeps the GiffGaff PAYG SIM active.
//
// GiffGaff requires at least one outgoing chargeable text every 6 months to
// keep the SIM active. This goroutine checks once per day whether >5 months
// have passed since the last chargeable outgoing SMS. If so, it sends a
// keepalive text and emails the admin.
//
// "Chargeable" means any outgoing SMS except source='balance_check'.
// The keepalive itself (source='keepalive') IS a chargeable text and resets
// the counter when the send queue processor marks it as sent.

import (
	"context"
	"fmt"
	"log"
	"time"

	"marlowfm.co.uk/sms-gateway/internal/database"
	"marlowfm.co.uk/sms-gateway/internal/email"
)

const (
	keepaliveNumber = "+447734139947"
	keepaliveText   = "Marlow FM Chargable Text"
	keepaliveMonths = 5
)

// runSIMKeepalive ticks hourly, runs its check once per calendar day (UK time),
// and sends a keepalive text if >5 months have passed since the last chargeable SMS.
func runSIMKeepalive(ctx context.Context, db *database.DB, bridge *email.Bridge, logger *log.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()

	var lastCheckDate string

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		// Only check once per calendar day (UK time ≈ UTC+1).
		now := time.Now().UTC()
		today := now.Add(time.Hour).Format("2006-01-02")
		if today == lastCheckDate {
			continue
		}
		lastCheckDate = today

		lastSent, err := db.LastChargeableSMSAt()
		if err != nil {
			logger.Printf("SIM keepalive: failed to get last chargeable SMS time: %v", err)
			lastCheckDate = "" // retry next hour
			continue
		}

		// No chargeable SMS history — use today as the baseline so we don't
		// fire immediately on a fresh install.
		if lastSent.IsZero() {
			logger.Printf("SIM keepalive: no chargeable SMS history — setting today as baseline")
			db.SetHealth("last_chargeable_sms_at", now.Format(time.RFC3339))
			continue
		}

		threshold := lastSent.AddDate(0, keepaliveMonths, 0)
		if now.Before(threshold) {
			daysLeft := int(threshold.Sub(now).Hours() / 24)
			logger.Printf("SIM keepalive: last chargeable SMS was %s — keepalive due in ~%d days",
				lastSent.Format("2006-01-02"), daysLeft)
			continue
		}

		// Guard against double-sending if the daemon restarts on the same day.
		var count int
		db.QueryRow(
			`SELECT COUNT(*) FROM send_queue
			 WHERE source = 'keepalive'
			   AND created_at >= datetime('now', '-1 day')
			   AND status IN ('pending', 'sent')`,
		).Scan(&count)
		if count > 0 {
			logger.Printf("SIM keepalive: keepalive already sent/pending in the last 24h — skipping")
			continue
		}

		logger.Printf("SIM keepalive: >%d months since last chargeable SMS — sending keepalive to %s",
			keepaliveMonths, keepaliveNumber)
		_, err = db.EnqueueSMS(keepaliveNumber, keepaliveText, "keepalive", "")
		if err != nil {
			logger.Printf("SIM keepalive: failed to enqueue: %v", err)
			lastCheckDate = "" // retry next hour
			continue
		}

		if bridge != nil {
			subj := "SIM keepalive text sent"
			body := fmt.Sprintf(
				"A SIM keepalive text has been sent to keep the GiffGaff SIM active.\n\nTo: %s\nMessage: %q\nLast chargeable text: %s\n",
				keepaliveNumber, keepaliveText, lastSent.Format("02 Jan 2006 15:04 UTC"),
			)
			if err := bridge.SendAdminEmail(adminEmail, subj, body); err != nil {
				logger.Printf("SIM keepalive: notification email failed: %v", err)
			}
		}
	}
}
