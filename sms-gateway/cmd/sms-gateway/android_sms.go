package main

// android_sms.go — fallback SMS source that reads directly from the Android
// telephony database.
//
// Root cause: RILD on this Qualcomm MSM8916 device intercepts incoming SMS
// via QMI (Qualcomm MSM Interface) and stores them in the Android telephony
// database BEFORE writing to SIM/ME storage — or in some boot states, without
// writing to SIM/ME storage at all. The AT+CMGL poll therefore sees count=0
// even when messages have been delivered.
//
// Fix: open the Android mmssms.db read-only on every poll cycle and import
// any inbox messages with _id greater than the last one we processed. The last
// processed ID is persisted in daemon_health so it survives gateway restarts.

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"

	"marlowfm.co.uk/sms-gateway/internal/database"
	"marlowfm.co.uk/sms-gateway/internal/email"
)

const (
	androidSMSDB = "/data/data/com.android.providers.telephony/databases/mmssms.db"

	// smsTypeInbox is the Android SMS type value for received messages.
	smsTypeInbox = 1
)

// pollAndroidSMS reads inbox SMS from the Android telephony database that
// arrived after the last processed ID, imports them into our database, and
// forwards them via email. Returns the number of new messages imported.
//
// Errors opening or querying the Android db are non-fatal — the AT+CMGL path
// continues to run in parallel, so a temporary lock or missing file just means
// we'll retry next poll cycle.
func pollAndroidSMS(db *database.DB, bridge *email.Bridge, logger *log.Logger) int {
	lastID := db.GetAndroidLastSMSID()

	// Open read-only with a short busy timeout in case RILD is mid-write.
	adb, err := sql.Open("sqlite", fmt.Sprintf(
		"file:%s?mode=ro&_busy_timeout=2000", androidSMSDB,
	))
	if err != nil {
		logger.Printf("Android SMS db: open error: %v", err)
		return 0
	}
	defer adb.Close()

	adb.SetMaxOpenConns(1)

	rows, err := adb.Query(
		`SELECT _id, address, date, body FROM sms WHERE type = ? AND _id > ? ORDER BY _id ASC`,
		smsTypeInbox, lastID,
	)
	if err != nil {
		logger.Printf("Android SMS db: query error (last_id=%d): %v", lastID, err)
		return 0
	}
	defer rows.Close()

	imported := 0
	maxID := lastID

	for rows.Next() {
		var androidID int64
		var address string
		var dateMs int64
		var body string

		if err := rows.Scan(&androidID, &address, &dateMs, &body); err != nil {
			logger.Printf("Android SMS db: scan error: %v", err)
			continue
		}

		if androidID > maxID {
			maxID = androidID
		}

		// Use -2 as a sentinel sim_index meaning "sourced from Android db",
		// distinct from ≥0 (real SIM slot) and -1 (unknown).
		msgID, err := db.InsertMessage(address, body, -2, 0, 0, 0)
		if err != nil {
			logger.Printf("Android SMS db: insert error (android_id=%d): %v", androidID, err)
			continue
		}
		logger.Printf("Android SMS: imported from %s (android_id=%d)", address, androidID)
		imported++

		// Check for a GiffGaff balance response before normal forwarding.
		if handleIncomingBalanceResponse(db, bridge, logger, msgID, address, body) {
			logger.Printf("Android SMS: msg %d from %s handled as balance response", msgID, address)
			continue
		}

		// Service SMS (e.g. giffgaff) — admin-only, not the radio station inbox.
		if isServiceSender(address) {
			subj := fmt.Sprintf("Service SMS from %s", address)
			if err := bridge.SendAdminEmail(adminEmail, subj, body); err != nil {
				logger.Printf("Android SMS: admin forward error (msg %d): %v", msgID, err)
				db.IncrementForwardAttempts(msgID)
			} else {
				logger.Printf("Android SMS: forwarded service msg %d from %s to admin", msgID, address)
				db.MarkForwarded(msgID, "service-sms")
			}
			continue
		}

		// Forward immediately using the Android message timestamp.
		receivedAt := time.Unix(dateMs/1000, 0).UTC().Format(time.RFC3339)
		msg := database.Message{
			ID:         msgID,
			SIMIndex:   -2,
			Sender:     address,
			ReceivedAt: receivedAt,
			Body:       body,
		}
		if err := bridge.ForwardMessage(msg); err != nil {
			logger.Printf("Android SMS: forward error (msg %d): %v", msgID, err)
			db.IncrementForwardAttempts(msgID)
		} else {
			logger.Printf("Android SMS: forwarded msg %d from %s", msgID, address)
		}
	}

	if maxID > lastID {
		db.SetAndroidLastSMSID(maxID)
	}

	return imported
}
