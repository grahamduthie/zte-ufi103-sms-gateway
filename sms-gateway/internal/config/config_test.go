package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate_ValidConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.example.com"
	cfg.Email.Username = "user@example.com"
	cfg.Email.Password = "secret"
	cfg.Email.ForwardTo = "forward@example.com"
	cfg.AuthorisedSenders = []string{"sender@example.com"}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidate_MissingSMTPHost(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "" // explicitly clear the default
	cfg.Email.Username = "user@example.com"
	cfg.Email.Password = "secret"
	cfg.Email.ForwardTo = "forward@example.com"
	cfg.AuthorisedSenders = []string{"sender@example.com"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing smtp_host, got nil")
	}
	if err.Error() != "email.smtp_host is required" {
		t.Fatalf("expected 'email.smtp_host is required', got: %v", err)
	}
}

func TestValidate_MissingUsername(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.example.com"
	cfg.Email.Password = "secret"
	cfg.Email.ForwardTo = "forward@example.com"
	cfg.AuthorisedSenders = []string{"sender@example.com"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing username, got nil")
	}
}

func TestValidate_MissingPassword(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.example.com"
	cfg.Email.Username = "user@example.com"
	cfg.Email.ForwardTo = "forward@example.com"
	cfg.AuthorisedSenders = []string{"sender@example.com"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing password, got nil")
	}
}

func TestValidate_MissingForwardTo(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.example.com"
	cfg.Email.Username = "user@example.com"
	cfg.Email.Password = "secret"
	cfg.AuthorisedSenders = []string{"sender@example.com"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for missing forward_to, got nil")
	}
}

func TestValidate_EmptyAuthorisedSenders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.example.com"
	cfg.Email.Username = "user@example.com"
	cfg.Email.Password = "secret"
	cfg.Email.ForwardTo = "forward@example.com"
	cfg.AuthorisedSenders = []string{}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty authorised_senders, got nil")
	}
}

func TestValidate_NilAuthorisedSenders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.example.com"
	cfg.Email.Username = "user@example.com"
	cfg.Email.Password = "secret"
	cfg.Email.ForwardTo = "forward@example.com"
	cfg.AuthorisedSenders = nil

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for nil authorised_senders, got nil")
	}
}

func TestValidate_PollIntervalZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.example.com"
	cfg.Email.Username = "user@example.com"
	cfg.Email.Password = "secret"
	cfg.Email.ForwardTo = "forward@example.com"
	cfg.AuthorisedSenders = []string{"sender@example.com"}
	cfg.SMS.PollIntervalSec = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for poll_interval_seconds=0, got nil")
	}
}

func TestValidate_PollIntervalNegative(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.example.com"
	cfg.Email.Username = "user@example.com"
	cfg.Email.Password = "secret"
	cfg.Email.ForwardTo = "forward@example.com"
	cfg.AuthorisedSenders = []string{"sender@example.com"}
	cfg.SMS.PollIntervalSec = -5

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for poll_interval_seconds=-5, got nil")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SMS.PollIntervalSec != 10 {
		t.Fatalf("expected poll_interval_seconds=10, got %d", cfg.SMS.PollIntervalSec)
	}
	if cfg.SMS.Storage != "SM" {
		t.Fatalf("expected storage=SM, got %s", cfg.SMS.Storage)
	}
	if !cfg.SMS.DeleteAfterFwd {
		t.Fatal("expected DeleteAfterFwd=true")
	}
	if cfg.Web.ListenAddr != "0.0.0.0:80" {
		t.Fatalf("expected listen_addr=0.0.0.0:80, got %s", cfg.Web.ListenAddr)
	}
	if cfg.Database != "/data/sms-gateway/sms.db" {
		t.Fatalf("expected database=/data/sms-gateway/sms.db, got %s", cfg.Database)
	}
	if cfg.Email.SMTPPort != 587 {
		t.Fatalf("expected smtp_port=587, got %d", cfg.Email.SMTPPort)
	}
	if cfg.Email.IMAPPort != 993 {
		t.Fatalf("expected imap_port=993, got %d", cfg.Email.IMAPPort)
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := DefaultConfig()
	cfg.Email.SMTPHost = "smtp.test.com"
	cfg.Email.Username = "user@test.com"
	cfg.Email.Password = "testpass"
	cfg.Email.ForwardTo = "forward@test.com"
	cfg.AuthorisedSenders = []string{"sender@test.com"}

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.Email.SMTPHost != "smtp.test.com" {
		t.Fatalf("expected SMTPHost=smtp.test.com, got %s", loaded.Email.SMTPHost)
	}
	if loaded.AuthorisedSenders[0] != "sender@test.com" {
		t.Fatalf("expected authorised_senders[0]=sender@test.com, got %s", loaded.AuthorisedSenders[0])
	}
}

func TestLoad_NonExistentFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.json")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte(`{bad json`), 0644)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}
