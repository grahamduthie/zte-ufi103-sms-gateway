package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"marlowfm.co.uk/sms-gateway/internal/atcmd"
	"marlowfm.co.uk/sms-gateway/internal/config"
	"marlowfm.co.uk/sms-gateway/internal/database"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const authCookieName = "gw_auth"

type Server struct {
	addr       string
	db         *database.DB
	at         *atcmd.Session
	cfg        *config.Config
	configPath string
	startedAt  time.Time
	tmpl       *template.Template
	handler    http.Handler // set by Start() for testability
}

func NewServer(addr string, db *database.DB, at *atcmd.Session, cfg *config.Config, configPath string, startedAt time.Time) *Server {
	s := &Server{
		addr:       addr,
		db:         db,
		at:         at,
		cfg:        cfg,
		configPath: configPath,
		startedAt:  startedAt,
	}
	s.initTemplates()
	s.setupHandler()
	return s
}

// setupHandler creates the HTTP handler mux for testing.
func (s *Server) setupHandler() {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("/inbox", s.requireAuth(s.handleInbox))
	mux.HandleFunc("/sent", s.requireAuth(s.handleSent))
	mux.HandleFunc("/compose", s.requireAuth(s.handleCompose))
	mux.HandleFunc("/conversation", s.requireAuth(s.handleConversation))
	mux.HandleFunc("/settings", s.requireAuth(s.handleSettings))
	mux.HandleFunc("/restarting", s.handleRestarting)
	mux.HandleFunc("/status", s.requireAuth(s.handleStatus))

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	s.handler = mux
}

// requireAuth wraps an http.Handler, redirecting unauthenticated requests to /login.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, _ := r.Cookie(authCookieName); c == nil || c.Value != "1" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// Handler returns the HTTP handler for testing.
func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) initTemplates() {
	funcs := template.FuncMap{
		"timeAgo": func(ts string) string {
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return ts
			}
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%d min ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%d hr ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%d days ago", int(d.Hours()/24))
			}
		},
		"signalBars": func(bars int) string {
			result := ""
			for i := 0; i < bars; i++ {
				result += "▓"
			}
			for i := bars; i < 5; i++ {
				result += "░"
			}
			return result
		},
		"shortText": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "..."
		},
		"messageDate": func(ts string) string {
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return ts
			}
			return t.Format("15:04")
		},
		"messageFullDate": func(ts string) string {
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return ts
			}
			return t.Format("2 Jan 15:04")
		},
		"ukTime": func(ts string) string {
			if ts == "" {
				return "—"
			}
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return ts
			}
			// UK is UTC+0 (GMT) or UTC+1 (BST). Without timezone data on the
			// device we approximate: BST runs April–October, GMT November–March.
			// This is correct ~95% of the time (within a few days of the switch).
			utc := t.UTC()
			m := utc.Month()
			if m >= 4 && m <= 10 {
				utc = utc.Add(time.Hour) // BST
			}
			return utc.Format("15:04 2 Jan")
		},
	}

	// Parse each template file individually into the template set
	tmpl := template.New("").Funcs(funcs)
	sub, err := fs.Sub(embedded, "templates")
	if err != nil {
		panic(fmt.Sprintf("embed sub: %v", err))
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		panic(fmt.Sprintf("read dir: %v", err))
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".html") {
			continue
		}
		data, err := fs.ReadFile(sub, entry.Name())
		if err != nil {
			panic(fmt.Sprintf("read %s: %v", entry.Name(), err))
		}
		if _, err := tmpl.New(entry.Name()).Parse(string(data)); err != nil {
			panic(fmt.Sprintf("parse %s: %v", entry.Name(), err))
		}
	}

	s.tmpl = tmpl
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("/inbox", s.requireAuth(s.handleInbox))
	mux.HandleFunc("/sent", s.requireAuth(s.handleSent))
	mux.HandleFunc("/compose", s.requireAuth(s.handleCompose))
	mux.HandleFunc("/conversation", s.requireAuth(s.handleConversation))
	mux.HandleFunc("/settings", s.requireAuth(s.handleSettings))
	mux.HandleFunc("/restarting", s.handleRestarting)
	mux.HandleFunc("/status", s.requireAuth(s.handleStatus))

	// Static files
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	return http.ListenAndServe(s.addr, mux)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		r.ParseForm()
		if s.cfg.Web.AdminPassword != "" && r.FormValue("password") == s.cfg.Web.AdminPassword {
			http.SetCookie(w, &http.Cookie{
				Name:     authCookieName,
				Value:    "1",
				Path:     "/",
				HttpOnly: true,
				Expires:  time.Now().Add(24 * time.Hour),
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.tmpl.ExecuteTemplate(w, "login.html", map[string]interface{}{
		"Error": r.Method == "POST",
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	var signal atcmd.SignalInfo
	var netInfo atcmd.NetworkInfo
	if s.at != nil {
		signal = s.at.CachedSignal()
		netInfo = s.at.CachedNetworkInfo()
	}
	received, sent, pending, _ := s.db.CountMessages()
	monthly, _ := s.db.GetMonthlyCounts()
	lastRx, lastTx, _ := s.db.GetLastMessageTimes()
	recentIn, _ := s.db.GetRecentMessages(3)
	recentOut, _ := s.db.GetSentMessages(3)

	data := map[string]interface{}{
		"Title":     "Dashboard",
		"Signal":    signal,
		"Network":   netInfo,
		"Received":  received,
		"Sent":      sent,
		"Pending":   pending,
		"RecentIn":  recentIn,
		"RecentOut": recentOut,
		"Monthly":   monthly,
		"LastRx":    lastRx,
		"LastTx":    lastTx,
		"Uptime":    int(time.Since(s.startedAt).Seconds()),
	}
	if err := s.tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	page := 1
	fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
	if page < 1 {
		page = 1
	}

	limit := 20
	offset := (page - 1) * limit

	all, _ := s.db.GetRecentMessages(1000)
	total := len(all)

	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	pageMsgs := all[start:end]

	totalPages := (total + limit - 1) / limit
	if totalPages < 1 {
		totalPages = 1
	}

	data := map[string]interface{}{
		"Title":      "Inbox",
		"Messages":   pageMsgs,
		"Page":       page,
		"TotalPages": totalPages,
		"Total":      total,
		"PrevPage":   page - 1,
		"NextPage":   page + 1,
	}
	s.tmpl.ExecuteTemplate(w, "inbox.html", data)
}

func (s *Server) handleSent(w http.ResponseWriter, r *http.Request) {
	msgs, _ := s.db.GetSentMessages(50)
	data := map[string]interface{}{
		"Title":    "Sent",
		"Messages": msgs,
	}
	s.tmpl.ExecuteTemplate(w, "sent.html", data)
}

func (s *Server) handleCompose(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		s.handleComposePost(w, r)
		return
	}
	data := map[string]interface{}{
		"Title": "Compose SMS",
	}
	if pref := r.URL.Query().Get("to"); pref != "" {
		data["PrefillTo"] = pref
	}
	s.tmpl.ExecuteTemplate(w, "compose.html", data)
}

func (s *Server) handleComposePost(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	number := r.FormValue("to_number")
	body := r.FormValue("body")

	if len(body) > 160 {
		body = body[:160]
	}

	_, err := s.db.EnqueueSMS(number, body, "web", "")
	if err != nil {
		http.Error(w, "Failed to queue message: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// If there's a 'from' param, go back to that conversation
	if from := r.FormValue("from"); from != "" {
		http.Redirect(w, r, "/conversation?number="+from, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/conversation", http.StatusSeeOther)
}

func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	number := r.URL.Query().Get("number")

	if number != "" {
		// Single conversation thread
		s.handleConversationThread(w, r, number)
		return
	}

	// Conversation list — paginated
	page := 1
	fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
	pg, _ := s.db.GetConversationsPage(page, 30)

	// Get signal info for the nav bar
	var signal atcmd.SignalInfo
	if s.at != nil {
		signal = s.at.CachedSignal()
	}

	data := map[string]interface{}{
		"Title":         "Conversations",
		"Conversations": pg.Conversations,
		"Page":          pg.Page,
		"TotalPages":    pg.TotalPages,
		"Total":         pg.Total,
		"PrevPage":      pg.Page - 1,
		"NextPage":      pg.Page + 1,
		"Signal":        signal,
	}
	s.tmpl.ExecuteTemplate(w, "conversation.html", data)
}

func (s *Server) handleConversationThread(w http.ResponseWriter, r *http.Request, number string) {
	msgs, _ := s.db.GetConversation(number, 200)

	var signal atcmd.SignalInfo
	if s.at != nil {
		signal = s.at.CachedSignal()
	}

	data := map[string]interface{}{
		"Title":    "Conversation: " + number,
		"Number":   number,
		"Messages": msgs,
		"Signal":   signal,
	}
	s.tmpl.ExecuteTemplate(w, "conversation.html", data)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var signal atcmd.SignalInfo
	var netInfo atcmd.NetworkInfo
	if s.at != nil {
		signal = s.at.CachedSignal()
		netInfo = s.at.CachedNetworkInfo()
	}
	received, sent, pending, _ := s.db.CountMessages()
	health, _ := s.db.GetHealthStatus()

	status := map[string]interface{}{
		"signal_rssi":        signal.RSSI,
		"signal_dbm":         signal.DBM,
		"signal_bars":        signal.Bars,
		"operator":           netInfo.Operator,
		"registered":         netInfo.Registered,
		"roaming":            netInfo.Roaming,
		"messages_received":  received,
		"messages_sent":      sent,
		"messages_pending":   pending,
		"uptime_seconds":     int(time.Since(s.startedAt).Seconds()),
		"health":             health,
	}

	if s.at != nil {
		status["resp_buf_size_bytes"] = s.at.RespBufSize()
	} else {
		status["resp_buf_size_bytes"] = 0
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		s.handleSettingsPost(w, r)
		return
	}
	data := map[string]interface{}{
		"Title": "Settings",
		"Cfg":   s.cfg,
		"Saved": r.URL.Query().Get("saved") == "1",
	}
	if err := s.tmpl.ExecuteTemplate(w, "settings.html", data); err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	// Handle action buttons (restart gateway or reboot device)
	switch r.FormValue("action") {
	case "restart":
		// Kill ourselves — start.sh's crash-restart loop will restart us
		go func() {
			time.Sleep(500 * time.Millisecond)
			syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		}()
		http.Redirect(w, r, "/restarting", http.StatusSeeOther)
		return
	case "reboot":
		// Issue reboot via librank (we may not have root directly)
		go func() {
			time.Sleep(500 * time.Millisecond)
			exec.Command("/system/xbin/librank", "/system/bin/reboot").Run()
		}()
		http.Redirect(w, r, "/restarting", http.StatusSeeOther)
		return
	case "shutdown":
		// Power off cleanly — user must unplug and replug to turn back on
		go func() {
			time.Sleep(500 * time.Millisecond)
			exec.Command("/system/xbin/librank", "/system/bin/sh", "-c", "setprop sys.powerctl shutdown").Run()
		}()
		http.Redirect(w, r, "/restarting", http.StatusSeeOther)
		return
	}

	// Update in-memory config from form values (only fields shown on the page).
	s.cfg.Email.IMAPHost = strings.TrimSpace(r.FormValue("imap_host"))
	s.cfg.Email.SMTPHost = strings.TrimSpace(r.FormValue("smtp_host"))
	s.cfg.Email.Username = strings.TrimSpace(r.FormValue("username"))
	s.cfg.Email.ForwardTo = strings.TrimSpace(r.FormValue("forward_to"))
	s.cfg.Email.FromName = strings.TrimSpace(r.FormValue("from_name"))

	// Only update password if a new one was supplied.
	if pw := strings.TrimSpace(r.FormValue("password")); pw != "" {
		s.cfg.Email.Password = pw
	}

	// Authorised senders — one per line.
	if raw := strings.TrimSpace(r.FormValue("authorised_senders")); raw != "" {
		var senders []string
		for _, line := range strings.Split(raw, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				senders = append(senders, s)
			}
		}
		s.cfg.AuthorisedSenders = senders
	}

	// Numeric fields — ignore parse errors (keep current value).
	fmt.Sscanf(r.FormValue("imap_poll_interval"), "%d", &s.cfg.IMAPPollInterval)
	fmt.Sscanf(r.FormValue("sms_poll_interval"), "%d", &s.cfg.SMS.PollIntervalSec)
	fmt.Sscanf(r.FormValue("sms_max_reply_chars"), "%d", &s.cfg.SMSMaxReplyChars)

	// Persist to disk.
	if s.configPath != "" {
		if err := config.Save(s.configPath, s.cfg); err != nil {
			http.Error(w, "Failed to save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// handleRestarting shows a "restarting" page that polls /status until the gateway is back.
func (s *Server) handleRestarting(w http.ResponseWriter, r *http.Request) {
	restartingHTML := `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta http-equiv="refresh" content="2">
<title>Restarting — SMS Gateway</title>
<style>
  * { margin:0; padding:0; box-sizing:border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
         background:#f4f4f4; display:flex; align-items:center; justify-content:center;
         min-height:100vh; }
  .card { background:#fff; border-radius:12px; box-shadow:0 2px 16px rgba(0,0,0,0.1);
          padding:48px 32px; text-align:center; max-width:380px; width:90%%; }
  .spinner { width:48px; height:48px; border:4px solid #e5e7eb; border-top-color:#1a1a2e;
             border-radius:50%%; animation:spin 0.8s linear infinite; margin:0 auto 20px; }
  @keyframes spin { to { transform:rotate(360deg); } }
  h2 { color:#1a1a2e; font-size:1.2rem; margin-bottom:8px; }
  p { color:#6b7280; font-size:.9rem; }
  .dots::after { content:''; animation:dots 1.5s steps(3,end) infinite; }
  @keyframes dots { 0%%{content:''} 33%%{content:'.'} 66%%{content:'..'} 100%%{content:'...'} }
  .countdown { margin-top:16px; font-size:.8rem; color:#9ca3af; }
</style>
</head>
<body>
<div class="card">
  <div class="spinner"></div>
  <h2>Gateway Restarting</h2>
  <p>Settings saved. Restarting SMS gateway<span class="dots"></span></p>
  <p class="countdown">You will be redirected automatically.</p>
</div>
<script>
  // Poll /status every 3 seconds — when it responds, redirect to dashboard
  var attempts = 0;
  function check() {
    attempts++;
    fetch('/status', { method: 'HEAD' })
      .then(function(r) {
        if (r.ok) {
          window.location.href = '/';
        }
      })
      .catch(function() {
        // still down, keep spinning
      });
  }
  setInterval(check, 3000);
  // Also redirect after 30 seconds as a safety net
  setTimeout(function() { window.location.href = '/'; }, 30000);
</script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, restartingHTML)
}
