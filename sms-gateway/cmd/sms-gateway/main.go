package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"marlowfm.co.uk/sms-gateway/internal/atcmd"
	"marlowfm.co.uk/sms-gateway/internal/config"
	"marlowfm.co.uk/sms-gateway/internal/database"
	"marlowfm.co.uk/sms-gateway/internal/email"
	"marlowfm.co.uk/sms-gateway/internal/web"
)

var version = "0.1.0"

// maxSendAttempts is the total number of delivery attempts before a queued
// SMS is permanently marked failed and a notification email is sent.
const maxSendAttempts = 50

func main() {
	configPath := flag.String("config", "/data/sms-gateway/config.json", "Path to config file")
	testMode := flag.Bool("test", false, "Run connectivity tests and exit")
	testEmail := flag.Bool("test-email", false, "Test email connectivity and exit")
	testCount := flag.Bool("test-count", false, "Test GetSMSCount and exit")
	testSend := flag.Bool("test-send", false, "Test SendSMS and exit")
	testDiag := flag.Bool("test-diag", false, "Run full modem diagnostics and exit")
	showVersion := flag.Bool("version", false, "Show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("sms-gateway", version)
		os.Exit(0)
	}

	logger := log.New(os.Stdout, "[sms-gateway] ", log.LstdFlags)

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Printf("Config not found at %s, using defaults", *configPath)
		cfg = config.DefaultConfig()
	}

	// Validate config
	if err := cfg.Validate(); err != nil {
		logger.Fatalf("Config error: %v", err)
	}

	// Open database
	db, err := database.Open(cfg.Database)
	if err != nil {
		logger.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()
	db.Migrate()
	db.CreateIndexes()
	logger.Printf("Database opened at %s", cfg.Database)

	// Test mode
	if *testMode || *testEmail || *testCount || *testSend || *testDiag {
		if *testDiag {
			runDiagTest(logger)
			return
		}
		if *testCount {
			runCountTest(cfg, db, logger)
			return
		}
		if *testSend {
			at, err := atcmd.NewSession("/dev/smd11")
			if err != nil {
				logger.Fatalf("AT session failed: %v", err)
			}
			defer at.Close()
			runSendTest(at, logger)
			return
		}
		runTests(cfg, db, logger)
		return
	}

	// Remove malformed send_queue entries (e.g. empty to_number from bad web form posts).
	db.Exec(`DELETE FROM send_queue WHERE status = 'pending' AND (to_number = '' OR to_number IS NULL)`)

	// Open AT command session
	at, err := atcmd.NewSession("/dev/smd11")
	if err != nil {
		logger.Fatalf("Failed to open /dev/smd11: %v", err)
	}
	defer at.Close()
	logger.Println("Connected to /dev/smd11")

	// Give the session the PIN for automatic re-lock recovery.
	if cfg.SMS.SIMPIN != "" {
		at.SetSIMPIN(cfg.SMS.SIMPIN)
	}

	// Unlock SIM if a PIN is configured.
	if cfg.SMS.SIMPIN != "" {
		locked, err := at.GetPINStatus()
		if err != nil {
			logger.Printf("Warning: could not check SIM PIN status: %v", err)
		}
		if locked {
			if err := at.UnlockSIM(cfg.SMS.SIMPIN); err != nil {
				logger.Fatalf("SIM PIN unlock failed: %v", err)
			}
			logger.Println("SIM unlocked")
		} else {
			logger.Println("SIM is not PIN-locked")
		}
	}

	// Apply modem settings once at startup: text mode, SM storage, AT+CNMI=2,1,0,0,0.
	// AT+CNMI is NOT re-applied on every poll (it triggers RILD to send AT+CPMS
	// on every cycle, which injects into SMS sends). RILD only sets CNMI=0,0,0,0,0
	// at boot; our gateway starts after RILD's init completes, so one application
	// at startup is sufficient. SetTextMode also runs in the hourly housekeeping.
	if err := at.SetTextMode(cfg.SMS.Storage); err != nil {
		logger.Printf("Warning: modem init (SetTextMode) failed: %v — will retry on first poll", err)
	}

	// Create email bridge
	emailCfg := email.EmailConfig{
		IMAPHost:          cfg.Email.IMAPHost,
		IMAPPort:          cfg.Email.IMAPPort,
		SMTPHost:          cfg.Email.SMTPHost,
		SMTPPort:          cfg.Email.SMTPPort,
		Username:          cfg.Email.Username,
		Password:          cfg.Email.Password,
		ForwardTo:         cfg.Email.ForwardTo,
		FromName:          cfg.Email.FromName,
		AuthorisedSenders: cfg.AuthorisedSenders,
	}
	bridge := email.NewBridge(emailCfg, db, logger)

	// Load the Marlow FM logo for email embedding
	if b64 := loadLogoBase64(); b64 != "" {
		email.SetLogoBase64(b64)
		logger.Println("Logo loaded for email embedding")
	}

	startedAt := time.Now()

	// Start web server
	webServer := web.NewServer(cfg.Web.ListenAddr, db, at, cfg, *configPath, startedAt)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("Web server recovered from panic: %v", r)
			}
		}()
		logger.Printf("Web server starting on %s", cfg.Web.ListenAddr)
		if err := webServer.Start(); err != nil {
			logger.Printf("Web server error: %v", err)
		}
	}()

	// Ignore SIGHUP so the daemon survives adb shell disconnect.
	signal.Ignore(syscall.SIGHUP)

	// Context and WaitGroup for graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	smsPollInterval := time.Duration(cfg.SMS.PollIntervalSec) * time.Second

	// Set package-level vars from config so goroutines don't need hardcoded values.
	adminEmail = cfg.Email.AdminEmail
	keepaliveNumber = cfg.SMS.KeepaliveNumber
	if adminEmail == "" {
		logger.Printf("Warning: email.admin_email not set — balance check and keepalive notifications will not be sent")
	}
	if keepaliveNumber == "" {
		logger.Printf("Warning: sms.keepalive_number not set — SIM keepalive texts will not be sent")
	}

	// Record startup time and reset any stale circuit breaker state.
	db.SetHealth("started_at", startedAt.UTC().Format(time.RFC3339))
	db.SetHealth("circuit_breaker", "closed")

	// Seed last_chargeable_sms_at into daemon_health from send_queue now, before
	// housekeeping has a chance to prune old records. Without this, a first-deploy
	// scenario where the last sent SMS is >90 days old could lose the timestamp.
	if t, err := db.LastChargeableSMSAt(); err == nil && !t.IsZero() {
		logger.Printf("SIM keepalive: last chargeable SMS was %s", t.Format("2006-01-02"))
	}

	logger.Println("SMS gateway started — polling for messages")

	// Initial SMS poll before starting tickers.
	processSMS(at, db, bridge, cfg, logger)

	// SMS poller with circuit breaker — exponential backoff on consecutive
	// AT failures, up to 60s max. Resets on any success.
	// Also selects on at.NewMessageCh: when the readerLoop sees a +CMTI:
	// unsolicited result (new SMS stored on SIM), we poll immediately instead
	// of waiting up to smsPollInterval. This closes the race window in which
	// RILD can read and delete a message before our gateway polls.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(smsPollInterval)
		defer t.Stop()
		var failCount int
		var backoffUntil time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-at.NewMessageCh:
				logger.Printf("SMS poller: +CMTI detected — immediate poll")
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Printf("SMS poller recovered from panic: %v", r)
					}
				}()

				// Circuit breaker: skip poll if still in backoff window.
				if time.Now().Before(backoffUntil) {
					return
				}

				ok := processSMS(at, db, bridge, cfg, logger)
				if !ok {
					failCount++
					// Backoff: 2s, 4s, 8s, 16s, 32s, 60s, 60s, …
					shift := failCount - 1
					if shift > 5 {
						shift = 5
					}
					backoff := time.Duration(2<<uint(shift)) * time.Second
					backoffUntil = time.Now().Add(backoff)
					db.SetHealth("circuit_breaker", fmt.Sprintf("open (fails=%d, retry in %v)", failCount, backoff))
					logger.Printf("SMS poll failed (%d consecutive), backing off %v", failCount, backoff)
				} else {
					if failCount > 0 {
						logger.Printf("SMS poll recovered after %d failures", failCount)
						db.SetHealth("circuit_breaker", "closed")
					}
					failCount = 0
				}
			}()
		}
	}()

	// Send queue processor — fires every 10s.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Printf("Send queue processor recovered from panic: %v", r)
					}
				}()
				processSendQueue(at, db, bridge, logger)
			}()
		}
	}()

	// IMAP IDLE — persistent connection to Ionos server. When a new email
	// arrives, the server pushes an unsolicited update, and we immediately
	// fetch and process it. Falls back to periodic polling if the server
	// doesn't support IDLE. Replaces the old 60-second poller.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("IMAP IDLE recovered from panic: %v", r)
			}
		}()
		bridge.IdleLoop(ctx)
	}()

	// WiFi watchdog — soft-reconnects wlan0 when wpa_supplicant drops the
	// connection, without touching the kernel module (rmmod/insmod).
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("WiFi watchdog recovered from panic: %v", r)
			}
		}()
		runWiFiWatchdog(ctx, logger)
	}()

	// SIM keepalive — sends a chargeable text if >5 months since the last one.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("SIM keepalive recovered from panic: %v", r)
			}
		}()
		runSIMKeepalive(ctx, db, bridge, logger)
	}()

	// Balance checker — sends "INFO" to GiffGaff every Sunday ~10am UK time.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("Balance checker recovered from panic: %v", r)
			}
		}()
		runBalanceChecker(ctx, db, bridge, logger)
	}()

	// Scheduled reboot — daily reboot at configured time to recover from WiFi
	// driver crashes that would otherwise leave the web GUI unreachable.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("Scheduled reboot recovered from panic: %v", r)
			}
		}()
		runScheduledReboot(ctx, cfg, logger)
	}()

	// Housekeeping — log rotation, WAL checkpoint, old record pruning.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("Housekeeping recovered from panic: %v", r)
			}
		}()
		runHousekeeping(ctx, db, at, cfg.SMS.Storage, cfg.LogFile, logger)
	}()

	// Signal/network info poller — updates the cache used by web handlers so
	// they never need to block on the AT mutex. Runs every 30s.
	wg.Add(1)
	go func() {
		defer wg.Done()
		at.GetSignal()
		at.GetNetworkInfo()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Printf("Signal poller recovered from panic: %v", r)
					}
				}()
				at.GetSignal()
				at.GetNetworkInfo()
			}()
		}
	}()

	// Wait for SIGINT or SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Println("Shutting down gracefully...")
	cancel() // signal goroutines to stop

	// Wait up to 10s for goroutines to finish.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		logger.Println("All goroutines stopped cleanly")
	case <-time.After(10 * time.Second):
		logger.Println("Timed out waiting for goroutines — forcing exit")
	}
	at.Close()
}

// processSMS polls the SIM for new messages, imports them, and forwards
// unforwarded messages via email. Returns true on success (AT commands
// worked), false on AT-level failure (for the circuit breaker).
func processSMS(at *atcmd.Session, db *database.DB, bridge *email.Bridge, cfg *config.Config, logger *log.Logger) bool {
	// Proactive SIM unlock: check BEFORE any SMS operations. The SIM can
	// re-lock after a WiFi mode switch. However, AT+CPIN? can return ERROR
	// due to RILD interference even when the SIM is fine — so we don't
	// fail the poll on unlock error; we proceed to GetSMSCount and let
	// that be the real indicator of SIM health.
	if cfg.SMS.SIMPIN != "" {
		if err := at.EnsureUnlocked(); err != nil {
			logger.Printf("SMS poll: SIM unlock check failed (%v) — continuing anyway, GetSMSCount will tell", err)
		}
	}

	count, err := at.GetSMSCount(cfg.SMS.Storage)
	if err != nil {
		logger.Printf("AT+CPMS error: %v", err)
		db.SetHealth("sms_status", fmt.Sprintf("error: %v", err))
		return false
	}
	db.SetHealth("sms_status", fmt.Sprintf("ok (count=%d)", count))
	db.SetHealth("last_poll_time", time.Now().UTC().Format(time.RFC3339))

	if count > 0 {
		logger.Printf("SMS poll: %d message(s) on SIM", count)
		msgs, err := at.ListSMS(cfg.SMS.Storage)
		if err != nil {
			logger.Printf("ListSMS error: %v", err)
		} else {
			logger.Printf("SMS poll: ListSMS returned %d messages", len(msgs))
			for _, msg := range msgs {
				exists, err := db.MessageExistsBySIMIndex(msg.Index)
				if err != nil {
					logger.Printf("DB dedup check error for index %d: %v", msg.Index, err)
					continue
				}
				if exists {
					continue
				}

				msgID, err := db.InsertMessage(msg.Sender, msg.Text, msg.Index)
				if err != nil {
					logger.Printf("Insert error for msg %d: %v", msg.Index, err)
					continue
				}
				logger.Printf("Imported SMS from %s (SIM index %d)", msg.Sender, msg.Index)
				handleIncomingBalanceResponse(db, bridge, logger, msgID, msg.Sender, msg.Text)

				if cfg.SMS.DeleteAfterFwd {
					if err := at.DeleteSMS(msg.Index); err != nil {
						logger.Printf("Delete SIM error for msg %d: %v", msg.Index, err)
					} else {
						db.MarkDeletedFromSIM(msgID)
					}
				}
			}
		}
	}

	// Also check the Android telephony database. RILD on this Qualcomm device
	// intercepts incoming SMS via QMI and stores them in mmssms.db, bypassing
	// SIM storage. This catches any messages the AT+CMGL poll missed.
	if n := pollAndroidSMS(db, bridge, logger); n > 0 {
		logger.Printf("Android SMS poll: imported %d message(s)", n)
	}

	// Forward all messages that haven't been emailed yet.
	unforwarded, err := db.GetUnforwardedMessages()
	if err != nil {
		logger.Printf("DB error fetching unforwarded: %v", err)
		return true // AT worked; DB error is not an AT failure
	}
	for _, msg := range unforwarded {
		if isServiceSender(msg.Sender) {
			// Service SMS (e.g. giffgaff) — admin-only, not the radio station inbox.
			subj := fmt.Sprintf("Service SMS from %s", msg.Sender)
			if bridge != nil {
				if err := bridge.SendAdminEmail(adminEmail, subj, msg.Body); err != nil {
					logger.Printf("Admin forward error for service message %d: %v", msg.ID, err)
					db.IncrementForwardAttempts(msg.ID)
					continue
				}
			}
			logger.Printf("Forwarded service message %d from %s to admin", msg.ID, msg.Sender)
			db.MarkForwarded(msg.ID, "service-sms")
		} else {
			if err := bridge.ForwardMessage(msg); err != nil {
				logger.Printf("Forward error for message %d: %v", msg.ID, err)
				db.IncrementForwardAttempts(msg.ID)
			} else {
				logger.Printf("Forwarded message %d from %s to email", msg.ID, msg.Sender)
			}
		}
	}
	return true
}

func processSendQueue(at *atcmd.Session, db *database.DB, bridge *email.Bridge, logger *log.Logger) {
	entries, err := db.GetPendingSendQueue()
	if err != nil {
		logger.Printf("Send queue error: %v", err)
		return
	}

	for _, entry := range entries {
		ref, err := at.SendSMS(entry.ToNumber, entry.Body)
		if err != nil {
			attempts := entry.Attempts + 1
			logger.Printf("Send SMS to %s failed (attempt %d/%d): %v", entry.ToNumber, attempts, maxSendAttempts, err)
			if attempts >= maxSendAttempts {
				reason := fmt.Sprintf("max attempts (%d): %v", maxSendAttempts, err)
				db.MarkSendQueueFailed(entry.ID, reason)
				logger.Printf("Giving up on SMS to %s after %d attempts", entry.ToNumber, maxSendAttempts)
				if bridge != nil && entry.Source != "keepalive" {
					if isGiffGafDest(entry.ToNumber) {
						body := fmt.Sprintf("Failed to send SMS to %s after %d attempts.\n\nMessage: %s\nReason: %s\n", entry.ToNumber, maxSendAttempts, entry.Body, reason)
						bridge.SendAdminEmail(adminEmail, "SMS send failed to "+entry.ToNumber, body)
					} else {
						if cerr := bridge.SendDeliveryConfirmation(entry.ToNumber, entry.Body, false, 0, reason, entry.SessionPrefix); cerr != nil {
							logger.Printf("Delivery confirmation email failed: %v", cerr)
						}
					}
				}
			} else {
				db.IncrementSendAttempts(entry.ID, entry.Attempts, err.Error())
			}
		} else {
			logger.Printf("SMS sent to %s (ref=%d)", entry.ToNumber, ref)
			db.MarkSendQueueSent(entry.ID, ref)
			db.SetHealth("last_send_time", time.Now().UTC().Format(time.RFC3339))
			// Track chargeable texts for SIM keepalive (balance_check SMSes don't count).
			if entry.Source != "balance_check" {
				db.SetHealth("last_chargeable_sms_at", time.Now().UTC().Format(time.RFC3339))
			}
			if bridge != nil && entry.Source != "keepalive" {
				if isGiffGafDest(entry.ToNumber) {
					// Texts to giffgaff — admin-only confirmation, not the radio station inbox.
					body := fmt.Sprintf("SMS sent to %s (ref=%d).\n\nMessage: %s\n", entry.ToNumber, ref, entry.Body)
					if cerr := bridge.SendAdminEmail(adminEmail, "SMS sent to "+entry.ToNumber, body); cerr != nil {
						logger.Printf("Admin confirmation email failed: %v", cerr)
					}
				} else {
					if cerr := bridge.SendDeliveryConfirmation(entry.ToNumber, entry.Body, true, ref, "", entry.SessionPrefix); cerr != nil {
						logger.Printf("Delivery confirmation email failed: %v", cerr)
					}
				}
			}
		}
	}
}

func runTests(cfg *config.Config, db *database.DB, logger *log.Logger) {
	logger.Println("Running connectivity tests...")

	at, err := atcmd.NewSession("/dev/smd11")
	if err != nil {
		logger.Fatalf("AT session failed: %v", err)
	}
	defer at.Close()

	signal, err := at.GetSignal()
	if err != nil {
		logger.Printf("AT+CSQ failed: %v", err)
	} else {
		logger.Printf("Signal: %d dBm (RSSI=%d, bars=%d)", signal.DBM, signal.RSSI, signal.Bars)
	}

	netInfo, err := at.GetNetworkInfo()
	if err != nil {
		logger.Printf("Network info failed: %v", err)
	} else {
		logger.Printf("Operator: %s, Registered: %v, IMSI: %s", netInfo.Operator, netInfo.Registered, netInfo.IMSI)
	}

	count, err := at.GetSMSCount(cfg.SMS.Storage)
	if err != nil {
		logger.Printf("SMS count failed: %v", err)
	} else {
		logger.Printf("SIM SMS count: %d", count)
	}

	bridge := email.NewBridge(email.EmailConfig{
		IMAPHost:  cfg.Email.IMAPHost,
		IMAPPort:  cfg.Email.IMAPPort,
		SMTPHost:  cfg.Email.SMTPHost,
		SMTPPort:  cfg.Email.SMTPPort,
		Username:  cfg.Email.Username,
		Password:  cfg.Email.Password,
		ForwardTo: cfg.Email.ForwardTo,
		FromName:  cfg.Email.FromName,
	}, db, logger)

	testMsg := database.Message{
		ID:         999,
		Sender:     "+447000000000",
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
		Body:       "Test message from sms-gateway",
	}
	if err := bridge.ForwardMessage(testMsg); err != nil {
		logger.Fatalf("Email forward test failed: %v", err)
	}
	logger.Println("Test email sent successfully")
	logger.Println("All tests passed")
}

func runCountTest(cfg *config.Config, db *database.DB, logger *log.Logger) {
	logger.Println("Testing GetSMSCount...")
	at, err := atcmd.NewSession("/dev/smd11")
	if err != nil {
		logger.Fatalf("AT session failed: %v", err)
	}
	defer at.Close()

	count, err := at.GetSMSCount(cfg.SMS.Storage)
	if err != nil {
		logger.Fatalf("GetSMSCount failed: %v", err)
	}
	logger.Printf("SMS count: %d in %s storage", count, cfg.SMS.Storage)
	logger.Println("GetSMSCount test passed")
}

func runSendTest(at *atcmd.Session, logger *log.Logger) {
	logger.Println("Testing SendSMS...")
	ref, err := at.SendSMS("+447700000001", fmt.Sprintf("Direct send test %s", time.Now().Format("15:04:05")))
	if err != nil {
		logger.Fatalf("SendSMS failed: %v", err)
	}
	logger.Printf("SMS sent! ref=%d", ref)
	logger.Println("SendSMS test passed")
}

// runDiagTest runs comprehensive modem diagnostics and prints human-readable output.
func runDiagTest(logger *log.Logger) {
	logger.Println("=== Modem Diagnostics ===")

	at, err := atcmd.NewSession("/dev/smd11")
	if err != nil {
		logger.Fatalf("AT session failed: %v", err)
	}
	defer at.Close()

	resp, err := at.SendRaw("AT", 3*time.Second)
	logResp("AT", resp, err)

	resp, err = at.SendRaw("AT+CPIN?", 3*time.Second)
	logResp("AT+CPIN?", resp, err)

	if resp != "" && strings.Contains(resp, "SIM PIN") {
		logger.Println("SIM is PIN-locked — unlocking with 8837")
		resp, err = at.SendRaw("AT+CPIN=\"8837\"", 5*time.Second)
		logResp("AT+CPIN=<PIN>", resp, err)
		if err != nil || (resp != "" && strings.Contains(resp, "ERROR")) {
			logger.Fatalf("PIN unlock failed — cannot continue diagnostics")
		}
		logger.Println("SIM unlocked successfully")
	}

	resp, err = at.SendRaw("AT+CREG?", 3*time.Second)
	logResp("AT+CREG?", resp, err)

	resp, err = at.SendRaw("AT+COPS?", 5*time.Second)
	logResp("AT+COPS?", resp, err)

	resp, err = at.SendRaw("AT+CSQ", 3*time.Second)
	logResp("AT+CSQ", resp, err)

	resp, err = at.SendRaw("AT+CNUM", 3*time.Second)
	logResp("AT+CNUM", resp, err)

	resp, err = at.SendRaw("AT+CSCA?", 3*time.Second)
	logResp("AT+CSCA?", resp, err)

	resp, err = at.SendRaw("AT+CPMS?", 3*time.Second)
	logResp("AT+CPMS?", resp, err)

	resp, err = at.SendRaw("AT+CMGF=1", 3*time.Second)
	logResp("AT+CMGF=1", resp, err)

	resp, err = at.SendRaw(`AT+CMGL="ALL"`, 5*time.Second)
	logResp("AT+CMGL=\"ALL\"", resp, err)

	netInfo, err := at.GetNetworkInfo()
	if err != nil {
		logger.Printf("GetNetworkInfo error: %v", err)
	} else {
		logger.Printf("Parsed network info: Registered=%v Roaming=%v Operator=%q IMSI=%q",
			netInfo.Registered, netInfo.Roaming, netInfo.Operator, netInfo.IMSI)
	}

	sig, err := at.GetSignal()
	if err != nil {
		logger.Printf("GetSignal error: %v", err)
	} else {
		logger.Printf("Parsed signal: RSSI=%d dBM=%d Bars=%d", sig.RSSI, sig.DBM, sig.Bars)
	}

	phone, err := at.GetPhoneNumber()
	if err != nil {
		logger.Printf("GetPhoneNumber error: %v", err)
	} else {
		logger.Printf("Phone number: %s", phone)
	}

	smsc, err := at.GetSMSC()
	if err != nil {
		logger.Printf("GetSMSC error: %v", err)
	} else {
		logger.Printf("SMSC: %s", smsc)
	}

	logger.Println("=== Diagnostics Complete ===")
}

func logResp(cmd, resp string, err error) {
	if err != nil {
		fmt.Printf("  %-20s ERROR: %v\n", cmd, err)
		if resp != "" {
			fmt.Printf("    partial: %q\n", truncateStr(resp, 200))
		}
	} else {
		fmt.Printf("  %-20s %q\n", cmd, truncateStr(resp, 300))
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// loadLogoBase64 reads the logo.png file and returns it as a base64 string.
// Returns empty string if the file is not found (logo is optional).
func loadLogoBase64() string {
	// Try the embedded static files first (dev/build environment)
	data, err := os.ReadFile("internal/web/static/logo.png")
	if err != nil {
		// Try the deployed path on the device
		data, err = os.ReadFile("/data/sms-gateway/logo.png")
		if err != nil {
			return ""
		}
	}
	return base64.StdEncoding.EncodeToString(data)
}
