package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	SMS               SMSConfig  `json:"sms"`
	Email             EmailConfig `json:"email"`
	WiFi              WiFiConfig  `json:"wifi"`
	Web               WebConfig   `json:"web"`
	AuthorisedSenders []string   `json:"authorised_senders"`
	SMSMaxReplyChars  int        `json:"sms_max_reply_chars"`
	IMAPPollInterval  int        `json:"imap_poll_interval_seconds"`
	Database          string     `json:"database"`
	LogFile           string     `json:"log_file"`
}

type SMSConfig struct {
	PollIntervalSec   int    `json:"poll_interval_seconds"`
	Storage           string `json:"storage"`
	DeleteAfterFwd    bool   `json:"delete_after_forward"`
	SIMPIN            string `json:"sim_pin"`            // optional — used to unlock PIN-locked SIM at startup
	KeepaliveNumber   string `json:"keepalive_number"`   // number to text for SIM keepalive (e.g. "+447xxxxxxxxx")
}

type EmailConfig struct {
	IMAPHost   string `json:"imap_host"`
	IMAPPort   int    `json:"imap_port"`
	SMTPHost   string `json:"smtp_host"`
	SMTPPort   int    `json:"smtp_port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	ForwardTo  string `json:"forward_to"`
	FromName   string `json:"from_name"`
	AdminEmail string `json:"admin_email"` // receives balance checks, keepalive notices, etc.
}

type WiFiConfig struct {
	Mode     string      `json:"mode"`
	Networks []WiFiNetCfg `json:"networks"`
}

type WiFiNetCfg struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
	Security string `json:"security"`
	Priority int    `json:"priority"`
}

type WebConfig struct {
	ListenAddr    string `json:"listen_addr"`
	AdminPassword string `json:"admin_password"`
}

func DefaultConfig() *Config {
	return &Config{
		SMS: SMSConfig{
			PollIntervalSec: 10,
			Storage:         "SM",
			DeleteAfterFwd:  true,
		},
		Email: EmailConfig{
			IMAPHost: "YOUR_IMAP_HOST",
			IMAPPort: 993,
			SMTPHost: "YOUR_SMTP_HOST",
			SMTPPort: 587,
			FromName: "Marlow FM SMS",
		},
		WiFi: WiFiConfig{
			Mode: "client",
		},
		Web: WebConfig{
			ListenAddr:    "0.0.0.0:80",
			AdminPassword: "", // must be set in config.json on the device
		},
		AuthorisedSenders: []string{"your-email@example.com"},
		SMSMaxReplyChars:  160,
		IMAPPollInterval:  60,
		Database:          "/data/sms-gateway/sms.db",
		LogFile:           "/data/sms-gateway/sms-gateway.log",
	}
}

// Validate checks that all required config fields are present and sane.
func (c *Config) Validate() error {
	if c.Email.SMTPHost == "" {
		return fmt.Errorf("email.smtp_host is required")
	}
	if c.Email.Username == "" {
		return fmt.Errorf("email.username is required")
	}
	if c.Email.Password == "" {
		return fmt.Errorf("email.password is required")
	}
	if c.Email.ForwardTo == "" {
		return fmt.Errorf("email.forward_to is required")
	}
	if len(c.AuthorisedSenders) == 0 {
		return fmt.Errorf("authorised_senders must contain at least one address")
	}
	if c.SMS.PollIntervalSec < 1 {
		return fmt.Errorf("sms.poll_interval_seconds must be >= 1")
	}
	if c.Database == "" {
		return fmt.Errorf("database path is required")
	}
	return nil
}

func Save(path string, cfg *Config) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "    ")
	return enc.Encode(cfg)
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := DefaultConfig()
	dec := json.NewDecoder(f)
	if err := dec.Decode(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
