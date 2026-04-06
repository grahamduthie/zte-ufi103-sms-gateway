package database

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("failed to open test DB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.Migrate()
	db.CreateIndexes()
	return db
}

func TestOpen_Close(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestOpen_NonExistentDir(t *testing.T) {
	_, err := Open("/nonexistent/path/test.db")
	if err == nil {
		t.Fatal("expected error for non-existent directory, got nil")
	}
}

func TestMigrate_CreatesTables(t *testing.T) {
	db := openTestDB(t)

	// Verify tables exist by performing operations
	_, err := db.InsertMessage("+447000000000", "Test message", 1)
	if err != nil {
		t.Fatalf("InsertMessage failed after migrate: %v", err)
	}
}

func TestInsertMessage_Success(t *testing.T) {
	db := openTestDB(t)

	id, err := db.InsertMessage("+447700000001", "Hello world", 1)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}
}

func TestInsertMessage_EmptyBody(t *testing.T) {
	db := openTestDB(t)

	id, err := db.InsertMessage("+447700000001", "", 1)
	if err != nil {
		t.Fatalf("InsertMessage with empty body failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID for empty body, got %d", id)
	}
}

func TestInsertMessage_EmptySender(t *testing.T) {
	db := openTestDB(t)

	id, err := db.InsertMessage("", "Hello world", 1)
	if err != nil {
		t.Fatalf("InsertMessage with empty sender failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID for empty sender, got %d", id)
	}
}

func TestMessageExistsBySIMIndex(t *testing.T) {
	db := openTestDB(t)

	exists, err := db.MessageExistsBySIMIndex(1)
	if err != nil {
		t.Fatalf("MessageExistsBySIMIndex failed: %v", err)
	}
	if exists {
		t.Fatal("expected message to not exist before insert")
	}

	_, err = db.InsertMessage("+447700000001", "Hello", 1)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}

	exists, err = db.MessageExistsBySIMIndex(1)
	if err != nil {
		t.Fatalf("MessageExistsBySIMIndex failed after insert: %v", err)
	}
	if !exists {
		t.Fatal("expected message to exist after insert")
	}
}

func TestMessageExistsBySIMIndex_NULL(t *testing.T) {
	db := openTestDB(t)

	// Insert and then mark as deleted (sets sim_index = NULL)
	id, err := db.InsertMessage("+447700000001", "Hello", 1)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}

	if err := db.MarkDeletedFromSIM(id); err != nil {
		t.Fatalf("MarkDeletedFromSIM failed: %v", err)
	}

	// After marking as deleted, sim_index is NULL so it should not exist
	exists, err := db.MessageExistsBySIMIndex(1)
	if err != nil {
		t.Fatalf("MessageExistsBySIMIndex failed: %v", err)
	}
	if exists {
		t.Fatal("expected message to not exist after SIM deletion (sim_index=NULL)")
	}
}

func TestMarkForwarded(t *testing.T) {
	db := openTestDB(t)

	id, err := db.InsertMessage("+447700000001", "Hello", 1)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}

	if err := db.MarkForwarded(id, "sess-123"); err != nil {
		t.Fatalf("MarkForwarded failed: %v", err)
	}

	msgs, err := db.GetUnforwardedMessages()
	if err != nil {
		t.Fatalf("GetUnforwardedMessages failed: %v", err)
	}
	for _, m := range msgs {
		if m.ID == id {
			t.Fatal("expected forwarded message to not appear in unforwarded list")
		}
	}
}

func TestMarkDeletedFromSIM(t *testing.T) {
	db := openTestDB(t)

	id, err := db.InsertMessage("+447700000001", "Hello", 5)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}

	if err := db.MarkDeletedFromSIM(id); err != nil {
		t.Fatalf("MarkDeletedFromSIM failed: %v", err)
	}

	// Verify sim_index is NULL
	exists, err := db.MessageExistsBySIMIndex(5)
	if err != nil {
		t.Fatalf("MessageExistsBySIMIndex failed: %v", err)
	}
	if exists {
		t.Fatal("expected sim_index to be NULL after MarkDeletedFromSIM")
	}
}

func TestCreateEmailSession(t *testing.T) {
	db := openTestDB(t)

	msgID, err := db.InsertMessage("+447700000001", "Hello", 1)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}

	if err := db.CreateEmailSession("060426-001", msgID, "+447700000001"); err != nil {
		t.Fatalf("CreateEmailSession failed: %v", err)
	}

	sender, err := db.LookupSenderByPrefix("060426")
	if err != nil {
		t.Fatalf("LookupSenderByPrefix failed: %v", err)
	}
	if sender != "+447700000001" {
		t.Fatalf("expected sender +447700000001, got %s", sender)
	}
}

func TestLookupSenderByPrefix_NotFound(t *testing.T) {
	db := openTestDB(t)

	_, err := db.LookupSenderByPrefix("nonexistent")
	if err == nil {
		t.Fatal("expected error for non-existent prefix, got nil")
	}
}

func TestEnqueueSMS(t *testing.T) {
	db := openTestDB(t)

	id, err := db.EnqueueSMS("+447700000001", "Reply message", "email_reply")
	if err != nil {
		t.Fatalf("EnqueueSMS failed: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}
}

func TestEnqueueSMS_EmptyNumber(t *testing.T) {
	db := openTestDB(t)

	_, err := db.EnqueueSMS("", "Reply message", "email_reply")
	if err == nil {
		t.Fatal("expected error for empty number, got nil")
	}
}

func TestGetPendingSendQueue(t *testing.T) {
	db := openTestDB(t)

	_, err := db.EnqueueSMS("+447700000001", "Message 1", "email_reply")
	if err != nil {
		t.Fatalf("EnqueueSMS failed: %v", err)
	}
	_, err = db.EnqueueSMS("+447700000001", "Message 2", "email_reply")
	if err != nil {
		t.Fatalf("EnqueueSMS failed: %v", err)
	}

	queue, err := db.GetPendingSendQueue()
	if err != nil {
		t.Fatalf("GetPendingSendQueue failed: %v", err)
	}
	if len(queue) != 2 {
		t.Fatalf("expected 2 pending messages, got %d", len(queue))
	}
}

func TestGetPendingSendQueue_Empty(t *testing.T) {
	db := openTestDB(t)

	queue, err := db.GetPendingSendQueue()
	if err != nil {
		t.Fatalf("GetPendingSendQueue failed: %v", err)
	}
	if len(queue) != 0 {
		t.Fatalf("expected 0 pending messages, got %d", len(queue))
	}
}

func TestMarkSendQueueSent(t *testing.T) {
	db := openTestDB(t)

	id, err := db.EnqueueSMS("+447700000001", "Test", "email_reply")
	if err != nil {
		t.Fatalf("EnqueueSMS failed: %v", err)
	}

	if err := db.MarkSendQueueSent(id, 42); err != nil {
		t.Fatalf("MarkSendQueueSent failed: %v", err)
	}

	queue, err := db.GetPendingSendQueue()
	if err != nil {
		t.Fatalf("GetPendingSendQueue failed: %v", err)
	}
	if len(queue) != 0 {
		t.Fatal("expected sent message to not appear in pending queue")
	}
}

func TestMarkSendQueueFailed(t *testing.T) {
	db := openTestDB(t)

	id, err := db.EnqueueSMS("+447700000001", "Test", "email_reply")
	if err != nil {
		t.Fatalf("EnqueueSMS failed: %v", err)
	}

	if err := db.MarkSendQueueFailed(id, "modem error"); err != nil {
		t.Fatalf("MarkSendQueueFailed failed: %v", err)
	}

	queue, err := db.GetPendingSendQueue()
	if err != nil {
		t.Fatalf("GetPendingSendQueue failed: %v", err)
	}
	if len(queue) != 0 {
		t.Fatal("expected failed message to not appear in pending queue")
	}
}

func TestIncrementSendAttempts(t *testing.T) {
	db := openTestDB(t)

	id, err := db.EnqueueSMS("+447700000001", "Test", "email_reply")
	if err != nil {
		t.Fatalf("EnqueueSMS failed: %v", err)
	}

	if err := db.IncrementSendAttempts(id, 0, "timeout"); err != nil {
		t.Fatalf("IncrementSendAttempts failed: %v", err)
	}

	// Verify the row was updated — query directly to bypass retry_at filter
	var attempts int
	var failureReason string
	err = db.QueryRow(`SELECT attempts, COALESCE(failure_reason,'') FROM send_queue WHERE id = ?`, id).Scan(&attempts, &failureReason)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected attempts=1, got %d", attempts)
	}
	if failureReason != "timeout" {
		t.Fatalf("expected failure_reason='timeout', got %q", failureReason)
	}
}

func TestGetUnforwardedMessages(t *testing.T) {
	db := openTestDB(t)

	// Insert and don't forward
	_, err := db.InsertMessage("+447700000001", "Unforwarded", 1)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}

	// Insert and forward
	id2, err := db.InsertMessage("+447700000001", "Forwarded", 2)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}
	if err := db.MarkForwarded(id2, "sess-1"); err != nil {
		t.Fatalf("MarkForwarded failed: %v", err)
	}

	unforwarded, err := db.GetUnforwardedMessages()
	if err != nil {
		t.Fatalf("GetUnforwardedMessages failed: %v", err)
	}
	if len(unforwarded) != 1 {
		t.Fatalf("expected 1 unforwarded message, got %d", len(unforwarded))
	}
	if unforwarded[0].Body != "Unforwarded" {
		t.Fatalf("expected body 'Unforwarded', got %q", unforwarded[0].Body)
	}
}

func TestSetHealth_GetHealthStatus(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetHealth("test_key", "test_value"); err != nil {
		t.Fatalf("SetHealth failed: %v", err)
	}

	status, err := db.GetHealthStatus()
	if err != nil {
		t.Fatalf("GetHealthStatus failed: %v", err)
	}
	if status["test_key"] != "test_value" {
		t.Fatalf("expected test_key=test_value, got %q", status["test_key"])
	}
}

func TestCountMessages(t *testing.T) {
	db := openTestDB(t)

	// Empty database
	received, _, _, err := db.CountMessages()
	count := received
	if err != nil {
		t.Fatalf("CountMessages failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 messages, got %d", count)
	}

	// Insert some messages
	for i := 0; i < 5; i++ {
		_, err := db.InsertMessage("+447700000001", "Test", i+1)
		if err != nil {
			t.Fatalf("InsertMessage failed: %v", err)
		}
	}

	received, _, _, err = db.CountMessages()
		count = received
	if err != nil {
		t.Fatalf("CountMessages failed: %v", err)
	}
	if count != 5 {
		t.Fatalf("expected 5 messages, got %d", count)
	}
}

func TestGetRecentMessages(t *testing.T) {
	db := openTestDB(t)

	// Insert 10 messages
	for i := 0; i < 10; i++ {
		_, err := db.InsertMessage("+447700000001", "Test", i+1)
		if err != nil {
			t.Fatalf("InsertMessage failed: %v", err)
		}
	}

	// Verify we got 5 messages (order among same-timestamp messages is undefined)
	msgs, err := db.GetRecentMessages(5)
	if err != nil {
		t.Fatalf("GetRecentMessages failed: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("expected 5 recent messages, got %d", len(msgs))
	}

	// Verify all returned messages are from our set of 10
	idSet := make(map[int64]bool)
	for i := int64(1); i <= 10; i++ {
		idSet[i] = true
	}
	for _, m := range msgs {
		if !idSet[m.ID] {
			t.Fatalf("message ID %d not in expected range 1-10", m.ID)
		}
	}
}

func TestGetSentMessages(t *testing.T) {
	db := openTestDB(t)

	// Insert and mark as sent
	id, err := db.EnqueueSMS("+447700000001", "Sent SMS", "web_ui")
	if err != nil {
		t.Fatalf("EnqueueSMS failed: %v", err)
	}
	if err := db.MarkSendQueueSent(id, 42); err != nil {
		t.Fatalf("MarkSendQueueSent failed: %v", err)
	}

	sent, err := db.GetSentMessages(10)
	if err != nil {
		t.Fatalf("GetSentMessages failed: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(sent))
	}
	if sent[0].ToNumber != "+447700000001" {
		t.Fatalf("expected to_number +447700000001, got %s", sent[0].ToNumber)
	}
}

func TestGetFailedSendQueue(t *testing.T) {
	db := openTestDB(t)

	// Insert a message
	id, err := db.EnqueueSMS("+447700000001", "Will fail", "email_reply")
	if err != nil {
		t.Fatalf("EnqueueSMS failed: %v", err)
	}

	// Simulate the main.go behaviour: increment attempts then mark as failed
	for i := 0; i < 5; i++ {
		err = db.IncrementSendAttempts(id, i, "timeout")
		if err != nil {
			t.Fatalf("IncrementSendAttempts failed at attempt %d: %v", i, err)
		}
	}

	// Mark as permanently failed (this is done by main.go when attempts >= maxSendAttempts)
	if err := db.MarkSendQueueFailed(id, "max attempts reached"); err != nil {
		t.Fatalf("MarkSendQueueFailed failed: %v", err)
	}

	failed, err := db.GetFailedSendQueue(10)
	if err != nil {
		t.Fatalf("GetFailedSendQueue failed: %v", err)
	}
	if len(failed) < 1 {
		t.Fatal("expected at least 1 failed message")
	}
}

func TestDatabaseConcurrency(t *testing.T) {
	db := openTestDB(t)

	// Concurrent inserts should work (SQLite serialises them)
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(n int) {
			_, err := db.InsertMessage("+447700000001", "Concurrent", n)
			if err != nil {
				t.Errorf("concurrent insert failed: %v", err)
			}
			done <- true
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}

	received, _, _, err := db.CountMessages()
	count := received
	if err != nil {
		t.Fatalf("CountMessages failed: %v", err)
	}
	if count != 10 {
		t.Fatalf("expected 10 messages, got %d", count)
	}
}

func TestConfigValidationViaDB(t *testing.T) {
	// Test that the database doesn't corrupt config data
	db := openTestDB(t)

	// Store a long health value
	longValue := time.Now().UTC().Format(time.RFC3339Nano)
	if err := db.SetHealth("timestamp", longValue); err != nil {
		t.Fatalf("SetHealth with long value failed: %v", err)
	}

	status, err := db.GetHealthStatus()
	if err != nil {
		t.Fatalf("GetHealthStatus failed: %v", err)
	}
	if status["timestamp"] != longValue {
		t.Fatalf("expected timestamp %q, got %q", longValue, status["timestamp"])
	}
}

func TestDatabaseCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.db")
	os.WriteFile(path, []byte("this is not a sqlite database"), 0644)

	_, err := Open(path)
	if err == nil {
		t.Fatal("expected error opening corrupt database, got nil")
	}
}

func TestDatabase_WALMode(t *testing.T) {
	db := openTestDB(t)

	// Verify WAL mode is enabled
	var journalMode string
	err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		t.Fatalf("PRAGMA journal_mode failed: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %s", journalMode)
	}
}

func TestDatabase_IntegrityCheck(t *testing.T) {
	db := openTestDB(t)

	err := db.CheckIntegrity()
	if err != nil {
		t.Fatalf("CheckIntegrity failed on fresh database: %v", err)
	}
}

func TestConversation_Empty(t *testing.T) {
	db := openTestDB(t)

	convos, err := db.GetConversations()
	if err != nil {
		t.Fatalf("GetConversations failed: %v", err)
	}
	if len(convos) != 0 {
		t.Fatalf("expected 0 conversations, got %d", len(convos))
	}
}

func TestConversation_SingleContact_InboundOnly(t *testing.T) {
	db := openTestDB(t)

	_, err := db.InsertMessage("+447700111111", "Hello from you", 1)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}
	_, err = db.InsertMessage("+447700111111", "Another message", 2)
	if err != nil {
		t.Fatalf("InsertMessage failed: %v", err)
	}

	convos, err := db.GetConversations()
	if err != nil {
		t.Fatalf("GetConversations failed: %v", err)
	}
	if len(convos) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(convos))
	}
	c := convos[0]
	if c.Number != "+447700111111" {
		t.Fatalf("expected number +447700111111, got %s", c.Number)
	}
	if c.TotalCount != 2 {
		t.Fatalf("expected 2 messages, got %d", c.TotalCount)
	}
	if c.UnreadCount != 2 {
		t.Fatalf("expected 2 unread, got %d", c.UnreadCount)
	}
}

func TestConversation_MixedInboundOutbound(t *testing.T) {
	db := openTestDB(t)

	// Different contact first (older)
	db.InsertMessage("+447700222222", "Hey!", 3)
	time.Sleep(1100 * time.Millisecond)
	// Now build the multi-message conversation (newer, should sort first)
	db.InsertMessage("+447700111111", "Hi there", 1)
	time.Sleep(1100 * time.Millisecond)
	// Outbound
	db.EnqueueSMS("+447700111111", "Hello back", "web")
	time.Sleep(1100 * time.Millisecond)
	// Another inbound
	db.InsertMessage("+447700111111", "How are you?", 2)

	convos, err := db.GetConversations()
	if err != nil {
		t.Fatalf("GetConversations failed: %v", err)
	}
	if len(convos) != 2 {
		t.Fatalf("expected 2 conversations, got %d", len(convos))
	}

	// First conversation should be the most recent one (+447700111111 with 3 msgs)
	c1 := convos[0]
	if c1.Number != "+447700111111" {
		t.Fatalf("expected first conversation +447700111111, got %s", c1.Number)
	}
	if c1.TotalCount != 3 {
		t.Fatalf("expected 3 messages in thread, got %d", c1.TotalCount)
	}

	// Get the full thread
	thread, err := db.GetConversation("+447700111111", 50)
	if err != nil {
		t.Fatalf("GetConversation failed: %v", err)
	}
	if len(thread) != 3 {
		t.Fatalf("expected 3 thread messages, got %d", len(thread))
	}
	// Most recent first: "How are you?" (in), "Hello back" (out), "Hi there" (in)
	if thread[0].Direction != "in" || thread[0].Body != "How are you?" {
		t.Fatalf("expected most recent to be inbound 'How are you?', got %+v", thread[0])
	}
	if thread[1].Direction != "out" || thread[1].Body != "Hello back" {
		t.Fatalf("expected second to be outbound 'Hello back', got %+v", thread[1])
	}
	if thread[2].Direction != "in" || thread[2].Body != "Hi there" {
		t.Fatalf("expected third to be inbound 'Hi there', got %+v", thread[2])
	}
}

func TestConversation_UnreadCountAfterForward(t *testing.T) {
	db := openTestDB(t)

	id, _ := db.InsertMessage("+447700111111", "Hello", 1)
	db.MarkForwarded(id, "session123")

	convos, _ := db.GetConversations()
	if len(convos) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(convos))
	}
	if convos[0].UnreadCount != 0 {
		t.Fatalf("expected 0 unread after forward, got %d", convos[0].UnreadCount)
	}
}

// ── NextDailySequence tests ──────────────────────────────────────────────

func TestNextDailySequence_FirstCall(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	seq := db.NextDailySequence(0)
	// Should be today's date in DDMMYY format with sequence 001
	today := time.Now().UTC().Format("020106")
	expected := today + "-001"
	if seq != expected {
		t.Fatalf("expected %q, got %q", expected, seq)
	}
}

func TestNextDailySequence_Increments(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	today := time.Now().UTC().Format("020106")
	seq1 := db.NextDailySequence(0)
	seq2 := db.NextDailySequence(0)
	seq3 := db.NextDailySequence(0)

	if seq1 != today+"-001" {
		t.Fatalf("seq1: expected %q-001, got %q", today, seq1)
	}
	if seq2 != today+"-002" {
		t.Fatalf("seq2: expected %q-002, got %q", today, seq2)
	}
	if seq3 != today+"-003" {
		t.Fatalf("seq3: expected %q-003, got %q", today, seq3)
	}
}

func TestNextDailySequence_DifferentTimezones(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// BST (UTC+1)
	seq := db.NextDailySequence(1)
	todayBST := time.Now().UTC().Add(1 * time.Hour).Format("020106")
	expected := todayBST + "-001"
	if seq != expected {
		t.Fatalf("expected %q, got %q", expected, seq)
	}
}
