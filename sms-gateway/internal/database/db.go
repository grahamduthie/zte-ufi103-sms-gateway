package database

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

// Message represents an SMS stored in the database.
type Message struct {
	ID              int64
	SIMIndex        int
	Sender          string
	ReceivedAt      string
	Body            string
	ForwardedAt     sql.NullString
	ForwardAttempts int
	EmailSessionID  sql.NullString
	DeletedFromSIM  bool
	ConcatRef       int // 0 = not a concatenated part
	ConcatTotal     int // 0 or 1 = single-part message
	ConcatPart      int // 0 = single-part, 1-based sequence number
}

// SendQueueEntry represents a pending outbound SMS.
type SendQueueEntry struct {
	ID            int64
	ToNumber      string
	Body          string
	CreatedAt     string
	Status        string
	SentAt        sql.NullString
	FailureReason sql.NullString
	ModemRef      sql.NullInt64
	Source        string
	Attempts      int
	SessionPrefix string
}

// Open opens (or creates) the SQLite database and initialises the schema.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if err := initSchema(db); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}

	// SQLite only supports one writer at a time; a single connection avoids
	// "database is locked" errors while keeping idle connections alive.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	return &DB{db}, nil
}

func initSchema(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS messages (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    sim_index         INTEGER,
    sender            TEXT NOT NULL,
    received_at       TEXT NOT NULL,
    body              TEXT NOT NULL,
    forwarded_at      TEXT,
    forward_attempts  INTEGER DEFAULT 0,
    email_session_id  TEXT,
    session_prefix    TEXT,
    deleted_from_sim  INTEGER DEFAULT 0,
    concat_ref        INTEGER DEFAULT 0,
    concat_total      INTEGER DEFAULT 0,
    concat_part       INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS email_sessions (
    session_id     TEXT PRIMARY KEY,
    session_prefix TEXT NOT NULL,
    message_id     INTEGER NOT NULL REFERENCES messages(id),
    sender_number  TEXT NOT NULL,
    created_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS send_queue (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    to_number      TEXT NOT NULL,
    body           TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending',
    sent_at        TEXT,
    failure_reason TEXT,
    modem_ref      INTEGER,
    source         TEXT NOT NULL DEFAULT 'email_reply',
    attempts       INTEGER DEFAULT 0,
    next_retry_at  TEXT,
    session_prefix TEXT
);

CREATE TABLE IF NOT EXISTS daemon_health (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS wifi_networks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ssid       TEXT NOT NULL UNIQUE,
    password   TEXT NOT NULL,
    security   TEXT NOT NULL DEFAULT 'WPA2-PSK',
    priority   INTEGER DEFAULT 0,
    auto_join  INTEGER DEFAULT 1
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`
	_, err := db.Exec(schema)
	return err
}

// Migrate adds columns that were introduced after the initial schema.
// Safe to call repeatedly — ALTER TABLE errors are silently ignored.
func (d *DB) Migrate() {
	// next_retry_at added for send-queue exponential backoff (2026-04-04)
	d.Exec(`ALTER TABLE send_queue ADD COLUMN next_retry_at TEXT`)
	// session_prefix added for email threading on delivery confirmations (2026-04-07)
	d.Exec(`ALTER TABLE send_queue ADD COLUMN session_prefix TEXT`)
	// concat_* added for multi-part SMS reassembly (2026-04-14)
	d.Exec(`ALTER TABLE messages ADD COLUMN concat_ref INTEGER DEFAULT 0`)
	d.Exec(`ALTER TABLE messages ADD COLUMN concat_total INTEGER DEFAULT 0`)
	d.Exec(`ALTER TABLE messages ADD COLUMN concat_part INTEGER DEFAULT 0`)
}

// RetroactiveConcatAssignment assigns concat metadata to old messages that
// were received before UDH parsing was implemented. It uses a heuristic:
// messages from the same sender arriving within 5 seconds of each other are
// treated as parts of a split multi-part SMS.
//
// Safe to call repeatedly — only processes messages with concat_ref = 0.
func (d *DB) RetroactiveConcatAssignment() error {
	// Find pairs of messages from the same sender within 5 seconds.
	rows, err := d.Query(`
		SELECT id, sender, received_at, body
		FROM messages
		WHERE concat_ref = 0
		ORDER BY sender, received_at ASC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type msg struct {
		id    int64
		sender string
		time  time.Time
		body  string
	}

	var all []msg
	for rows.Next() {
		var m msg
		var ts string
		if err := rows.Scan(&m.id, &m.sender, &ts, &m.body); err != nil {
			return err
		}
		// received_at is stored as RFC3339 text; parse it for time arithmetic.
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			m.time = t
		} else if t, err := time.Parse("2006-01-02T15:04:05Z07:00", ts); err == nil {
			m.time = t
		} else {
			m.time = time.Now() // fallback: treat as isolated message
		}
		all = append(all, m)
	}

	// Group by sender and find adjacent pairs within 5 seconds.
	type senderGroup struct {
		sender string
		groups [][]msg
	}
	senderMsgs := make(map[string][]msg)
	for _, m := range all {
		senderMsgs[m.sender] = append(senderMsgs[m.sender], m)
	}

	for sender, msgs := range senderMsgs {
		_ = sender
		i := 0
		for i < len(msgs) {
			j := i + 1
			// Extend group while next message is within 5 seconds of previous.
			for j < len(msgs) {
				dt := msgs[j].time.Sub(msgs[j-1].time)
				if dt > 5*time.Second {
					break
				}
				j++
			}
			groupSize := j - i
			if groupSize > 1 {
				// Assign concat_ref (use a hash of sender + timestamp as unique ref)
				ref := int(msgs[i].time.Unix() % 65536)
				if ref <= 0 {
					ref = 1
				}
				for k := 0; k < groupSize; k++ {
					part := k + 1
					_, _ = d.Exec(
						`UPDATE messages SET concat_ref = ?, concat_total = ?, concat_part = ? WHERE id = ?`,
						ref, groupSize, part, msgs[i+k].id,
					)
				}
			}
			i = j
		}
	}
	return nil
}

// Create indexes (idempotent with IF NOT EXISTS workaround via try)
func (d *DB) CreateIndexes() {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_unforwarded ON messages(forwarded_at)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_received ON messages(received_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sender_time ON messages(sender, received_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_prefix ON email_sessions(session_prefix)`,
		`CREATE INDEX IF NOT EXISTS idx_queue_pending ON send_queue(status, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_queue_tonumber ON send_queue(to_number)`,
		`CREATE INDEX IF NOT EXISTS idx_queue_tonumber_time ON send_queue(to_number, COALESCE(sent_at, created_at) DESC)`,
	}
	for _, idx := range indexes {
		d.Exec(idx) // ignore errors (may already exist)
	}
}

// InsertMessage saves an incoming SMS to the database.
func (d *DB) InsertMessage(sender, body string, simIndex int, concatRef, concatTotal, concatPart int) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := d.Exec(
		`INSERT INTO messages (sim_index, sender, received_at, body, deleted_from_sim, concat_ref, concat_total, concat_part)
         VALUES (?, ?, ?, ?, 0, ?, ?, ?)`,
		simIndex, sender, now, body, concatRef, concatTotal, concatPart,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MarkForwarded records that a message has been forwarded via email.
func (d *DB) MarkForwarded(id int64, sessionID string) error {
	prefix := ""
	if len(sessionID) >= 6 {
		prefix = sessionID[:6]
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.Exec(
		`UPDATE messages SET forwarded_at = ?, email_session_id = ?, session_prefix = ? WHERE id = ?`,
		now, sessionID, prefix, id,
	)
	return err
}

// MarkDeletedFromSIM marks a message as deleted from the SIM and clears its
// sim_index so that reused SIM slots can be imported as new messages.
func (d *DB) MarkDeletedFromSIM(id int64) error {
	_, err := d.Exec(`UPDATE messages SET deleted_from_sim = 1, sim_index = NULL WHERE id = ?`, id)
	return err
}

// CreateEmailSession records a session for reply routing.
func (d *DB) CreateEmailSession(sessionID string, messageID int64, senderNumber string) error {
	prefix := ""
	if len(sessionID) >= 6 {
		prefix = sessionID[:6]
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.Exec(
		`INSERT OR REPLACE INTO email_sessions (session_id, session_prefix, message_id, sender_number, created_at)
         VALUES (?, ?, ?, ?, ?)`,
		sessionID, prefix, messageID, senderNumber, now,
	)
	return err
}

// LookupSenderByPrefix finds the original SMS sender from a reply's session prefix.
func (d *DB) LookupSenderByPrefix(prefix string) (string, error) {
	var sender string
	err := d.QueryRow(
		`SELECT es.sender_number FROM email_sessions es
         JOIN messages m ON es.message_id = m.id
         WHERE es.session_prefix = ?`,
		prefix,
	).Scan(&sender)
	return sender, err
}

// GetUnforwardedMessages returns messages that haven't been forwarded yet.
func (d *DB) GetUnforwardedMessages() ([]Message, error) {
	rows, err := d.Query(
		`SELECT id, COALESCE(sim_index, -1), sender, received_at, body,
			 COALESCE(forward_attempts,0), deleted_from_sim,
			 COALESCE(concat_ref,0), COALESCE(concat_total,0), COALESCE(concat_part,0)
         FROM messages WHERE forwarded_at IS NULL ORDER BY received_at ASC LIMIT 50`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		err := rows.Scan(&m.ID, &m.SIMIndex, &m.Sender, &m.ReceivedAt, &m.Body,
			&m.ForwardAttempts, &m.DeletedFromSIM,
			&m.ConcatRef, &m.ConcatTotal, &m.ConcatPart)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// IncrementForwardAttempts increments the forward attempt counter.
func (d *DB) IncrementForwardAttempts(id int64) error {
	_, err := d.Exec(`UPDATE messages SET forward_attempts = forward_attempts + 1 WHERE id = ?`, id)
	return err
}

// GetPendingSendQueue returns pending outbound SMS messages whose retry
// time has passed (or that have never been attempted).
func (d *DB) GetPendingSendQueue() ([]SendQueueEntry, error) {
	rows, err := d.Query(
		`SELECT id, to_number, body, created_at, status, COALESCE(sent_at,''),
                COALESCE(failure_reason,''), COALESCE(modem_ref,0), source, COALESCE(attempts,0),
                COALESCE(session_prefix,'')
         FROM send_queue
         WHERE status = 'pending'
           AND (next_retry_at IS NULL OR next_retry_at <= datetime('now'))
         ORDER BY created_at ASC LIMIT 10`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []SendQueueEntry
	for rows.Next() {
		var e SendQueueEntry
		err := rows.Scan(&e.ID, &e.ToNumber, &e.Body, &e.CreatedAt, &e.Status,
			&e.SentAt, &e.FailureReason, &e.ModemRef, &e.Source, &e.Attempts, &e.SessionPrefix)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// MarkSendQueueSent marks a queued message as sent.
func (d *DB) MarkSendQueueSent(id int64, modemRef int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.Exec(
		`UPDATE send_queue SET status = 'sent', sent_at = ?, modem_ref = ? WHERE id = ?`,
		now, modemRef, id,
	)
	return err
}

// MarkSendQueueFailed permanently marks a queued message as failed (no more retries).
func (d *DB) MarkSendQueueFailed(id int64, reason string) error {
	_, err := d.Exec(
		`UPDATE send_queue SET status = 'failed', failure_reason = ?, attempts = attempts + 1 WHERE id = ?`,
		reason, id,
	)
	return err
}

// IncrementSendAttempts records a transient failure and schedules the next
// retry using exponential backoff: 10s, 20s, 40s, 80s … capped at 5 minutes.
func (d *DB) IncrementSendAttempts(id int64, attempts int, reason string) error {
	// backoff = 10s * 2^attempts, capped at 300s (5 min)
	backoffSec := 10 * (1 << uint(attempts))
	if backoffSec > 300 {
		backoffSec = 300
	}
	_, err := d.Exec(
		`UPDATE send_queue
         SET attempts = attempts + 1,
             failure_reason = ?,
             next_retry_at = datetime('now', '+' || ? || ' seconds')
         WHERE id = ?`,
		reason, backoffSec, id,
	)
	return err
}

// EnqueueSMS adds a new outbound SMS to the send queue.
func (d *DB) EnqueueSMS(toNumber, body, source, sessionPrefix string) (int64, error) {
	if toNumber == "" {
		return 0, fmt.Errorf("to_number must not be empty")
	}
	if body == "" {
		return 0, fmt.Errorf("body must not be empty")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := d.Exec(
		`INSERT INTO send_queue (to_number, body, created_at, source, session_prefix) VALUES (?, ?, ?, ?, ?)`,
		toNumber, body, now, source, sessionPrefix,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetHealth writes a key-value pair to daemon_health.
func (d *DB) SetHealth(key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.Exec(
		`INSERT OR REPLACE INTO daemon_health (key, value, updated_at) VALUES (?, ?, ?)`,
		key, value, now,
	)
	return err
}

// mergeConcatParts merges consecutive inbound messages from the same sender
// with matching concat_ref into a single message with a combined body.
// Parts are merged in concat_part order. Messages without concat_ref (0) are
// passed through unchanged.
func mergeConcatParts(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}

	type concatKey struct{ sender string; ref int }
	type concatGroup struct {
		firstIdx int
		total    int
		parts    map[int]*Message // part number -> message
	}

	// Scan for concat groups.
	groups := make(map[concatKey]*concatGroup)
	for i, m := range msgs {
		if m.ConcatRef > 0 && m.ConcatTotal > 1 {
			key := concatKey{m.Sender, m.ConcatRef}
			g, ok := groups[key]
			if !ok {
				g = &concatGroup{firstIdx: i, parts: make(map[int]*Message)}
				groups[key] = g
			}
			if m.ConcatTotal > g.total {
				g.total = m.ConcatTotal
			}
			g.parts[m.ConcatPart] = &msgs[i]
		}
	}

	// Build merged result.
	var result []Message
	skip := make(map[int]bool) // indices to skip (merged into another)

	// For each group, check completeness and merge if all parts present.
	for key, g := range groups {
		if len(g.parts) != g.total {
			continue // incomplete — leave as separate messages
		}

		// All parts present — merge them.
		var body strings.Builder
		firstMsg := g.parts[1]
		for p := 1; p <= g.total; p++ {
			part := g.parts[p]
			if p > 1 {
				body.WriteString("\n")
			}
			body.WriteString(part.Body)
			skip[g.firstIdx] = true // we'll mark each part's index below
		}

		// Use the first part's message as the merged result.
		firstMsg.Body = body.String()
		// Mark other parts as skipped.
		for p := 2; p <= g.total; p++ {
			// Find the index of this part in the original slice.
			for i, m := range msgs {
				if m.ConcatRef == key.ref && m.Sender == key.sender && m.ConcatPart == p {
					skip[i] = true
				}
			}
		}
	}

	for i, m := range msgs {
		if !skip[i] {
			result = append(result, m)
		}
	}
	return result
}

// GetRecentMessages returns the most recent received messages.
func (d *DB) GetRecentMessages(limit int) ([]Message, error) {
	rows, err := d.Query(
		`SELECT id, COALESCE(sim_index, -1), sender, received_at, body,
                COALESCE(forwarded_at,''), COALESCE(forward_attempts,0),
                COALESCE(email_session_id,''), deleted_from_sim,
                COALESCE(concat_ref,0), COALESCE(concat_total,0), COALESCE(concat_part,0)
         FROM messages ORDER BY received_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var fwd sql.NullString
		var esid sql.NullString
		err := rows.Scan(&m.ID, &m.SIMIndex, &m.Sender, &m.ReceivedAt, &m.Body,
			&fwd, &m.ForwardAttempts, &esid, &m.DeletedFromSIM,
			&m.ConcatRef, &m.ConcatTotal, &m.ConcatPart)
		if err != nil {
			return nil, err
		}
		m.ForwardedAt = fwd
		m.EmailSessionID = esid
		msgs = append(msgs, m)
	}
	return mergeConcatParts(msgs), nil
}

// GetSentMessages returns recently sent SMS from the send queue.
func (d *DB) GetSentMessages(limit int) ([]SendQueueEntry, error) {
	rows, err := d.Query(
		`SELECT id, to_number, body, created_at, status,
                COALESCE(sent_at,''), COALESCE(failure_reason,''),
                COALESCE(modem_ref,0), source, COALESCE(attempts,0)
         FROM send_queue WHERE status = 'sent' ORDER BY sent_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []SendQueueEntry
	for rows.Next() {
		var e SendQueueEntry
		var sentAt sql.NullString
		var failReason sql.NullString
		var modemRef sql.NullInt64
		err := rows.Scan(&e.ID, &e.ToNumber, &e.Body, &e.CreatedAt, &e.Status,
			&sentAt, &failReason, &modemRef, &e.Source, &e.Attempts)
		if err != nil {
			return nil, err
		}
		e.SentAt = sentAt
		e.FailureReason = failReason
		e.ModemRef = modemRef
		entries = append(entries, e)
	}
	return entries, nil
}

// MessageExistsBySIMIndex returns true if a non-deleted message with the given
// SIM index is already in the database (used to prevent duplicate imports).
func (d *DB) MessageExistsBySIMIndex(simIndex int) (bool, error) {
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE sim_index = ?`,
		simIndex,
	).Scan(&n)
	return n > 0, err
}

// GetHealthStatus returns all daemon_health key-value pairs.
func (d *DB) GetHealthStatus() (map[string]string, error) {
	rows, err := d.Query(`SELECT key, value FROM daemon_health`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// GetFailedSendQueue returns permanently failed outbound SMS entries.
func (d *DB) GetFailedSendQueue(limit int) ([]SendQueueEntry, error) {
	rows, err := d.Query(
		`SELECT id, to_number, body, created_at, status,
		        COALESCE(sent_at,''), COALESCE(failure_reason,''),
		        COALESCE(modem_ref,0), source, COALESCE(attempts,0)
		 FROM send_queue WHERE status = 'failed' ORDER BY created_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []SendQueueEntry
	for rows.Next() {
		var e SendQueueEntry
		var sentAt sql.NullString
		var failReason sql.NullString
		var modemRef sql.NullInt64
		err := rows.Scan(&e.ID, &e.ToNumber, &e.Body, &e.CreatedAt, &e.Status,
			&sentAt, &failReason, &modemRef, &e.Source, &e.Attempts)
		if err != nil {
			return nil, err
		}
		e.SentAt = sentAt
		e.FailureReason = failReason
		e.ModemRef = modemRef
		entries = append(entries, e)
	}
	return entries, nil
}

// CountMessages returns total received and sent counts.
func (d *DB) CountMessages() (received, sent, pending int, err error) {
	err = d.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&received)
	if err != nil {
		return
	}
	err = d.QueryRow(`SELECT COUNT(*) FROM send_queue WHERE status = 'sent'`).Scan(&sent)
	if err != nil {
		return
	}
	err = d.QueryRow(`SELECT COUNT(*) FROM send_queue WHERE status = 'pending'`).Scan(&pending)
	return
}

// GetAndroidLastSMSID returns the highest Android SMS _id that has already
// been imported, or 0 if no Android messages have been processed yet.
func (d *DB) GetAndroidLastSMSID() int64 {
	return d.getAndroidLastSMSID()
}

func (d *DB) getAndroidLastSMSID() int64 {
	var val string
	err := d.QueryRow(`SELECT value FROM daemon_health WHERE key = 'last_android_sms_id'`).Scan(&val)
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseInt(val, 10, 64)
	return id
}

// SetAndroidLastSMSID persists the highest Android SMS _id that has been
// imported, so the gateway does not re-import messages after a restart.
func (d *DB) SetAndroidLastSMSID(id int64) {
	d.SetHealth("last_android_sms_id", strconv.FormatInt(id, 10))
}

// GetHealth returns a single value from daemon_health, or sql.ErrNoRows if the key is absent.
func (d *DB) GetHealth(key string) (string, error) {
	var val string
	err := d.QueryRow(`SELECT value FROM daemon_health WHERE key = ?`, key).Scan(&val)
	return val, err
}

// LastChargeableSMSAt returns the timestamp of the most recent outgoing SMS
// that counts as a chargeable text (any source except 'balance_check').
// It checks the daemon_health cache first; if absent, initialises from the
// send_queue and persists the result. Returns zero time if there are no sent
// chargeable messages.
func (d *DB) LastChargeableSMSAt() (time.Time, error) {
	// Check cache first.
	val, err := d.GetHealth("last_chargeable_sms_at")
	if err == nil && val != "" {
		if t, perr := time.Parse(time.RFC3339, val); perr == nil {
			return t, nil
		}
	}

	// Initialise from send_queue.
	var sentAt sql.NullString
	err = d.QueryRow(
		`SELECT sent_at FROM send_queue
		 WHERE status = 'sent' AND source != 'balance_check'
		 ORDER BY sent_at DESC LIMIT 1`,
	).Scan(&sentAt)
	if err == sql.ErrNoRows || !sentAt.Valid {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}

	t, perr := time.Parse(time.RFC3339, sentAt.String)
	if perr != nil {
		return time.Time{}, perr
	}
	d.SetHealth("last_chargeable_sms_at", t.Format(time.RFC3339))
	return t, nil
}

// CheckIntegrity runs PRAGMA integrity_check on the database and returns
// an error if the database is corrupted.
func (d *DB) CheckIntegrity() error {
	var result string
	if err := d.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity check query: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("database integrity check: %s", result)
	}
	return nil
}

// SendQueueStats holds a snapshot of the send queue state.
type SendQueueStats struct {
	Pending       int
	Failed        int
	Sent          int
	OldestPending time.Time
	NextRetryAt   time.Time
}

// GetSendQueueStats returns a summary of the send queue state.
func (d *DB) GetSendQueueStats() (SendQueueStats, error) {
	var stats SendQueueStats

	err := d.QueryRow(`SELECT COUNT(*) FROM send_queue WHERE status = 'pending'`).Scan(&stats.Pending)
	if err != nil {
		return stats, fmt.Errorf("count pending: %w", err)
	}

	err = d.QueryRow(`SELECT COUNT(*) FROM send_queue WHERE status = 'failed'`).Scan(&stats.Failed)
	if err != nil {
		return stats, fmt.Errorf("count failed: %w", err)
	}

	err = d.QueryRow(`SELECT COUNT(*) FROM send_queue WHERE status = 'sent'`).Scan(&stats.Sent)
	if err != nil {
		return stats, fmt.Errorf("count sent: %w", err)
	}

	// Get oldest pending entry
	var oldest sql.NullString
	err = d.QueryRow(`SELECT created_at FROM send_queue WHERE status = 'pending' ORDER BY created_at ASC LIMIT 1`).Scan(&oldest)
	if err != nil && err != sql.ErrNoRows {
		return stats, fmt.Errorf("oldest pending: %w", err)
	}
	if oldest.Valid {
		stats.OldestPending, _ = time.Parse(time.RFC3339, oldest.String)
	}

	// Get next retry time (earliest next_retry_at among pending entries)
	var nextRetry sql.NullString
	err = d.QueryRow(`SELECT next_retry_at FROM send_queue WHERE status = 'pending' AND next_retry_at IS NOT NULL ORDER BY next_retry_at ASC LIMIT 1`).Scan(&nextRetry)
	if err != nil && err != sql.ErrNoRows {
		return stats, fmt.Errorf("next retry: %w", err)
	}
	if nextRetry.Valid {
		stats.NextRetryAt, _ = time.Parse(time.RFC3339, nextRetry.String)
	}

	return stats, nil
}

// ConversationSummary is a single row in the conversation list.
type ConversationSummary struct {
	Number      string
	LastName    string // short display name for this contact
	LastBody    string
	LastTime    string
	UnreadCount int // messages not yet forwarded
	TotalCount  int
}

// ThreadMessage is a single message in a conversation thread, inbound or outbound.
type ThreadMessage struct {
	Direction string // "in" or "out"
	Body      string
	Timestamp string
	Status    string // "forwarded", "pending", "sent", "failed"
	ConcatRef int    // 0 = not a concatenated part
	ConcatTotal int  // 0 or 1 = single-part message
	ConcatPart  int  // 0 = single-part, 1-based sequence number
}

// ConversationPage holds a page of conversation summaries plus metadata.
type ConversationPage struct {
	Conversations []ConversationSummary
	Total         int
	Page          int
	TotalPages    int
}

// GetConversationsPage returns a paginated list of unique contacts, sorted by last message time.
func (d *DB) GetConversationsPage(page, pageSize int) (ConversationPage, error) {
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * pageSize

	// First get total count
	var total int
	err := d.QueryRow(`
		SELECT COUNT(DISTINCT contact) FROM (
			SELECT sender AS contact FROM messages
			UNION
			SELECT to_number AS contact FROM send_queue
		)
	`).Scan(&total)
	if err != nil {
		return ConversationPage{TotalPages: 1}, err
	}

	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}

	rows, err := d.Query(`
		WITH all_msgs AS (
			SELECT sender AS contact, body, received_at AS ts, 'in' AS dir, forwarded_at AS fwd FROM messages
			UNION ALL
			SELECT to_number AS contact, body, COALESCE(sent_at, created_at) AS ts, 'out' AS dir, NULL AS fwd FROM send_queue
		),
		aggregated AS (
			SELECT
				contact,
				MAX(ts) AS sort_time,
				MAX(ts) AS last_time,
				SUM(CASE WHEN dir = 'in' AND fwd IS NULL THEN 1 ELSE 0 END) AS unread,
				COUNT(*) AS total
			FROM all_msgs
			GROUP BY contact
		)
		SELECT a.contact, a.last_time, a.unread, a.total,
			(SELECT body FROM all_msgs WHERE contact = a.contact ORDER BY ts DESC LIMIT 1) AS last_body
		FROM aggregated a
		ORDER BY a.sort_time DESC
		LIMIT ? OFFSET ?
	`, pageSize, offset)
	if err != nil {
		return ConversationPage{Total: total, TotalPages: totalPages}, err
	}
	defer rows.Close()

	var convos []ConversationSummary
	for rows.Next() {
		var c ConversationSummary
		if err := rows.Scan(&c.Number, &c.LastTime, &c.UnreadCount, &c.TotalCount, &c.LastBody); err != nil {
			return ConversationPage{Total: total, TotalPages: totalPages}, err
		}
		c.LastName = c.Number
		convos = append(convos, c)
	}

	return ConversationPage{
		Conversations: convos,
		Total:         total,
		Page:          page,
		TotalPages:    totalPages,
	}, nil
}

// GetConversations returns a list of all unique contacts, sorted by last message time.
// Deprecated: use GetConversationsPage for large datasets.
func (d *DB) GetConversations() ([]ConversationSummary, error) {
	p, err := d.GetConversationsPage(1, 10000)
	if err != nil {
		return nil, err
	}
	return p.Conversations, nil
}

// mergeConcatThreads merges inbound ThreadMessages with matching concat_ref
// into a single message with a combined body. Outbound messages are passed
// through unchanged.
func mergeConcatThreads(msgs []ThreadMessage) []ThreadMessage {
	if len(msgs) == 0 {
		return msgs
	}

	type concatKey struct{ sender string; ref int }
	type concatGroup struct {
		firstIdx int
		total    int
		parts    map[int]*ThreadMessage
	}

	groups := make(map[concatKey]*concatGroup)
	for i, m := range msgs {
		if m.Direction == "in" && m.ConcatRef > 0 && m.ConcatTotal > 1 {
			// sender is embedded in the key but we need a common sender
			// For conversation threads, all inbound msgs have the same sender.
			key := concatKey{"", m.ConcatRef}
			g, ok := groups[key]
			if !ok {
				g = &concatGroup{firstIdx: i, parts: make(map[int]*ThreadMessage)}
				groups[key] = g
			}
			if m.ConcatTotal > g.total {
				g.total = m.ConcatTotal
			}
			g.parts[m.ConcatPart] = &msgs[i]
		}
	}

	var result []ThreadMessage
	skip := make(map[int]bool)

	for key, g := range groups {
		if len(g.parts) != g.total {
			continue
		}
		var body strings.Builder
		firstMsg := g.parts[1]
		for p := 1; p <= g.total; p++ {
			part := g.parts[p]
			if p > 1 {
				body.WriteString("\n")
			}
			body.WriteString(part.Body)
		}
		firstMsg.Body = body.String()
		for p := 2; p <= g.total; p++ {
			for i, m := range msgs {
				if m.Direction == "in" && m.ConcatRef == key.ref && m.ConcatPart == p {
					skip[i] = true
				}
			}
		}
	}

	for i, m := range msgs {
		if !skip[i] {
			result = append(result, m)
		}
	}
	return result
}

// GetConversation returns all messages (inbound + outbound) for a single contact,
// in chronological order. Concatenated SMS parts with matching concat_ref are
// merged into a single message.
func (d *DB) GetConversation(number string, limit int) ([]ThreadMessage, error) {
	rows, err := d.Query(`
		SELECT 'in' AS direction, body, received_at AS ts,
			CASE WHEN forwarded_at IS NOT NULL THEN 'forwarded' ELSE 'pending' END AS status,
			COALESCE(concat_ref, 0), COALESCE(concat_total, 0), COALESCE(concat_part, 0)
		FROM messages
		WHERE sender = ?
		UNION ALL
		SELECT 'out' AS direction, body,
			COALESCE(sent_at, created_at) AS ts,
			status, 0, 0, 0
		FROM send_queue
		WHERE to_number = ?
		ORDER BY ts ASC
		LIMIT ?
	`, number, number, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []ThreadMessage
	for rows.Next() {
		var t ThreadMessage
		if err := rows.Scan(&t.Direction, &t.Body, &t.Timestamp, &t.Status,
			&t.ConcatRef, &t.ConcatTotal, &t.ConcatPart); err != nil {
			return nil, err
		}
		msgs = append(msgs, t)
	}
	return mergeConcatThreads(msgs), nil
}

// MonthlyCounts holds sent/received counts for the current UK calendar month.
type MonthlyCounts struct {
	Received int
	Sent     int
}

// GetMonthlyCounts returns received and sent counts for the current calendar month (UTC).
// On this device we use UTC because the IANA timezone database is not available.
func (d *DB) GetMonthlyCounts() (MonthlyCounts, error) {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthStartStr := monthStart.Format(time.RFC3339)

	var counts MonthlyCounts
	err := d.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN received_at >= ? THEN 1 ELSE 0 END), 0)
		FROM messages
	`, monthStartStr).Scan(&counts.Received)
	if err != nil {
		return counts, fmt.Errorf("count received: %w", err)
	}

	err = d.QueryRow(`
		SELECT COALESCE(SUM(CASE WHEN COALESCE(sent_at, created_at) >= ? THEN 1 ELSE 0 END), 0)
		FROM send_queue
		WHERE status = 'sent'
	`, monthStartStr).Scan(&counts.Sent)
	if err != nil {
		return counts, fmt.Errorf("count sent: %w", err)
	}

	return counts, nil
}

// GetLastMessageTimes returns the timestamp of the most recent received and sent messages.
func (d *DB) GetLastMessageTimes() (lastReceived, lastSent string, err error) {
	err = d.QueryRow(`SELECT received_at FROM messages ORDER BY received_at DESC LIMIT 1`).Scan(&lastReceived)
	if err == sql.ErrNoRows {
		err = nil
	} else if err != nil {
		return "", "", fmt.Errorf("last received: %w", err)
	}

	err = d.QueryRow(`SELECT COALESCE(sent_at, created_at) FROM send_queue WHERE status = 'sent' ORDER BY COALESCE(sent_at, created_at) DESC LIMIT 1`).Scan(&lastSent)
	if err == sql.ErrNoRows {
		err = nil
	} else if err != nil {
		return "", "", fmt.Errorf("last sent: %w", err)
	}

	return lastReceived, lastSent, nil
}

// NextDailySequence returns the next sequence number for today, formatted as
// a 3-digit zero-padded string (e.g. "001", "002"). The key is reset when the
// date changes. The returned string is the full session ID: "DDMMYY-NNN".
// tzOffset is the UTC offset in hours (e.g. 1 for BST, 0 for GMT).
func (d *DB) NextDailySequence(tzOffset int) string {
	now := time.Now().UTC().Add(time.Duration(tzOffset) * time.Hour)
	today := now.Format("020106")

	key := "sms_daily_seq_date"
	var storedDate string
	err := d.QueryRow(`SELECT value FROM daemon_health WHERE key = ?`, key).Scan(&storedDate)

	var seq int
	if err != nil || storedDate != today {
		// New day or first run — reset counter
		seq = 1
		d.SetHealth(key, today)
	} else {
		// Same day — increment counter
		countKey := "sms_daily_seq_count"
		var storedCount string
		err := d.QueryRow(`SELECT value FROM daemon_health WHERE key = ?`, countKey).Scan(&storedCount)
		if err != nil {
			seq = 1
		} else {
			seq, _ = strconv.Atoi(storedCount)
			seq++
		}
	}

	d.SetHealth("sms_daily_seq_count", strconv.Itoa(seq))
	return fmt.Sprintf("%s-%03d", today, seq)
}
