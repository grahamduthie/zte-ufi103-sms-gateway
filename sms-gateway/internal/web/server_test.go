package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"marlowfm.co.uk/sms-gateway/internal/config"
	"marlowfm.co.uk/sms-gateway/internal/database"
)

func newTestServer(t *testing.T) (*Server, func()) {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.json")

	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test DB: %v", err)
	}
	db.Migrate()
	db.CreateIndexes()

	cfg := config.DefaultConfig()
	cfg.Email.SMTPHost = "smtp.test.com"
	cfg.Email.Username = "user@test.com"
	cfg.Email.Password = "testpass"
	cfg.Email.ForwardTo = "forward@test.com"
	cfg.AuthorisedSenders = []string{"sender@test.com"}
	cfg.Database = dbPath

	// Write config
	cfgJSON, _ := json.Marshal(cfg)
	os.WriteFile(cfgPath, cfgJSON, 0644)

	startedAt := time.Now()

	// Create a nil AT session for web server tests
	// (we don't have /dev/smd11 in tests)
	srv := NewServer("127.0.0.1:0", db, nil, cfg, cfgPath, startedAt)

	cleanup := func() {
		db.Close()
	}

	return srv, cleanup
}

// authReq wraps a request with the auth cookie so it bypasses the login gate.
func authReq(method, path string, body io.Reader) *http.Request {
	t := httptest.NewRequest(method, path, body)
	t.AddCookie(&http.Cookie{Name: "gw_auth", Value: "1"})
	return t
}

// ── /status endpoint tests ───────────────────────────────────────────────

func TestStatusEndpoint_ReturnsJSON(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	req := authReq("GET", "/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
}

func TestStatusEndpoint_HealthFields(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	req := authReq("GET", "/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	// health map may be empty in tests — just verify the field exists
	health, ok := resp["health"].(map[string]interface{})
	if !ok {
		t.Fatal("missing health field in status response")
	}
	_ = health // health exists, contents depend on runtime state

	if _, ok := resp["messages_received"]; !ok {
		t.Error("missing messages_received field")
	}
	if _, ok := resp["signal_bars"]; !ok {
		t.Error("missing signal_bars field")
	}
	if _, ok := resp["operator"]; !ok {
		t.Error("missing operator field")
	}
}

func TestStatusEndpoint_Uptime(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	time.Sleep(10 * time.Millisecond)

	req := authReq("GET", "/status", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	uptime, ok := resp["uptime_seconds"].(float64)
	if !ok {
		t.Fatal("missing or invalid uptime_seconds field")
	}
	if uptime < 0 {
		t.Fatalf("expected uptime >= 0, got %.1f", uptime)
	}
}

// ── Dashboard endpoint tests ─────────────────────────────────────────────

func TestDashboard_ReturnsHTML(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	req := authReq("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType == "" {
		t.Error("missing Content-Type header")
	}
}

func TestDashboard_ContainsTitle(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	req := authReq("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if !contains(body, "Marlow FM SMS") {
		t.Error("dashboard page doesn't contain 'Marlow FM SMS'")
	}
}

// ── Inbox endpoint tests ─────────────────────────────────────────────────

func TestInbox_ReturnsHTML(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	req := authReq("GET", "/inbox", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestInbox_PageParameter(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	// Page 0 should work
	req := authReq("GET", "/inbox?page=0", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for page=0, got %d", w.Code)
	}

	// Page 1 should work
	req = authReq("GET", "/inbox?page=1", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for page=1, got %d", w.Code)
	}

	// Page 999 (no data) should still return 200
	req = authReq("GET", "/inbox?page=999", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for page=999, got %d", w.Code)
	}
}

// ── Sent endpoint tests ──────────────────────────────────────────────────

func TestSent_ReturnsHTML(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	req := authReq("GET", "/sent", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// ── Compose endpoint tests ───────────────────────────────────────────────

func TestCompose_GET_ReturnsForm(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	req := authReq("GET", "/compose", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestCompose_POST_EnqueuesMessage(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	body := strings.NewReader("to_number=%2B447700000001&body=Test+message+from+web")
	req := authReq("POST", "/compose", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Should redirect or return success
	if w.Code != http.StatusOK && w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("expected 200, 302, or 303, got %d", w.Code)
	}

	// Verify message was queued
	queue, err := srv.db.GetPendingSendQueue()
	if err != nil {
		t.Fatalf("GetPendingSendQueue failed: %v", err)
	}
	if len(queue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(queue))
	}
	if queue[0].ToNumber != "+447700000001" {
		t.Fatalf("expected to_number +447700000001, got %s", queue[0].ToNumber)
	}
}

func TestCompose_POST_EmptyNumber(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	body := strings.NewReader("to_number=&body=Test+message")
	req := authReq("POST", "/compose", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Should return error or redirect
	if w.Code == http.StatusOK {
		// Check if it's a form error (OK with error message) vs silent success
		body := w.Body.String()
		if contains(body, "queued") || contains(body, "success") {
			t.Error("empty number should not result in queued message")
		}
	}
}

func TestCompose_POST_EmptyMessage(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	body := strings.NewReader("to_number=%2B447700000001&body=")
	req := authReq("POST", "/compose", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Should return error
	if w.Code == http.StatusOK {
		body := w.Body.String()
		if contains(body, "queued") || contains(body, "success") {
			t.Error("empty message should not result in queued message")
		}
	}
}

// ── Settings endpoint tests ──────────────────────────────────────────────

func TestSettings_GET_ReturnsForm(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	req := authReq("GET", "/settings", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestSettings_POST_SavesConfig(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	formData := "smtp_host=smtp.test.com&username=user%40test.com" +
		"&password=testpass&forward_to=forward%40test.com" +
		"&from_name=Test+Name&imap_host=imap.test.com" +
		"&authorised_senders=sender%40test.com" +
		"&sms_poll_interval=3&sms_max_reply_chars=160" +
		"&imap_poll_interval=60"
	req := authReq("POST", "/settings", strings.NewReader(formData))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
		t.Fatalf("expected 200, 302, or 303, got %d", w.Code)
	}

	// Verify config was saved
	loaded, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatalf("failed to load config after save: %v", err)
	}
	if loaded.Email.SMTPHost != "smtp.test.com" {
		t.Fatalf("expected SMTPHost=smtp.test.com, got %s", loaded.Email.SMTPHost)
	}
}

// ── Unknown route tests ──────────────────────────────────────────────────

func TestUnknownRoute_RedirectsToDashboard(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	// Unauthenticated requests to unknown routes redirect to /login
	req := httptest.NewRequest("GET", "/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Should redirect to /login (303) since there's no auth cookie
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect to /login, got %d", w.Code)
	}
	if w.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %s", w.Header().Get("Location"))
	}
}

// ── Helper ────────────────────────────────────────────────────────────────

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
