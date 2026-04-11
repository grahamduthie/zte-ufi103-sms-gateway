package main

import (
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"marlowfm.co.uk/sms-gateway/internal/config"
	"marlowfm.co.uk/sms-gateway/internal/web"
)

// setupServer holds mutable WiFi network state across requests during setup mode.
type setupServer struct {
	mu         sync.Mutex
	networks   []config.WiFiNetCfg
	cfg        *config.Config
	configPath string
	logger     *log.Logger
	tmpl       *template.Template
}

// runSetupMode starts a minimal captive portal HTTP server on :80.
// No AT session, no SMS/IMAP/watchdog goroutines are started.
// Called by main() when --setup-mode flag is set.
func runSetupMode(cfg *config.Config, configPath string, logger *log.Logger) {
	tmpl, err := template.New("setup").Parse(setupHTML)
	if err != nil {
		logger.Fatalf("Setup mode: template parse error: %v", err)
	}

	nets := make([]config.WiFiNetCfg, len(cfg.WiFi.Networks))
	copy(nets, cfg.WiFi.Networks)

	s := &setupServer{
		networks:   nets,
		cfg:        cfg,
		configPath: configPath,
		logger:     logger,
		tmpl:       tmpl,
	}

	mux := http.NewServeMux()

	// Serve pico.min.css and logo.png from the embedded static FS.
	staticSub, _ := fs.Sub(web.StaticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Captive portal probe handlers — return redirects so the OS shows the portal UI.
	// /generate_204: Android expects HTTP 204 for a connected network; anything else
	// triggers the captive portal notification. We return a redirect.
	mux.HandleFunc("/generate_204", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://192.168.100.1/setup", http.StatusFound)
	})
	// iOS: captive.apple.com/hotspot-detect.html — redirect to setup.
	mux.HandleFunc("/hotspot-detect.html", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup", http.StatusFound)
	})
	// Windows: www.msftconnecttest.com/ncsi.txt — redirect to setup.
	mux.HandleFunc("/ncsi.txt", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup", http.StatusFound)
	})

	mux.HandleFunc("/setup", s.handleSetup)
	mux.HandleFunc("/setup/add", s.handleAdd)
	mux.HandleFunc("/setup/delete", s.handleDelete)
	mux.HandleFunc("/setup/save", s.handleSave)
	mux.HandleFunc("/rebooting", s.handleRebooting)

	// Catch-all: any unrecognised path → setup page.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup", http.StatusFound)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "SMS-Gateway")
		mux.ServeHTTP(w, r)
	})

	logger.Printf("Setup mode: captive portal on :80 — SMS-Gateway-Setup hotspot")
	if err := http.ListenAndServe(":80", handler); err != nil {
		logger.Printf("Setup server error: %v", err)
	}
}

// handleSetup renders the WiFi setup page.
func (s *setupServer) handleSetup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	nets := make([]config.WiFiNetCfg, len(s.networks))
	copy(nets, s.networks)
	s.mu.Unlock()

	data := map[string]interface{}{
		"Networks": nets,
		"Error":    r.URL.Query().Get("error"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		s.logger.Printf("Setup template error: %v", err)
	}
}

// handleAdd adds a network to the in-memory list and redirects back to /setup.
func (s *setupServer) handleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	r.ParseForm()
	ssid := strings.TrimSpace(r.FormValue("ssid"))
	psk := r.FormValue("psk")
	security := r.FormValue("security")
	if security == "" {
		security = "WPA2"
	}

	if ssid == "" {
		http.Redirect(w, r, "/setup?error=SSID+is+required", http.StatusSeeOther)
		return
	}
	if strings.ToUpper(security) != "OPEN" && len(psk) < 8 {
		http.Redirect(w, r, "/setup?error=Password+must+be+at+least+8+characters", http.StatusSeeOther)
		return
	}

	s.mu.Lock()
	priority := len(s.networks) + 1
	s.networks = append(s.networks, config.WiFiNetCfg{
		SSID:     ssid,
		Password: psk,
		Security: security,
		Priority: priority,
	})
	s.mu.Unlock()

	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}

// handleDelete removes a network by index and redirects back to /setup.
func (s *setupServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	r.ParseForm()
	idx, err := strconv.Atoi(r.FormValue("index"))
	if err != nil {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	s.mu.Lock()
	if idx >= 0 && idx < len(s.networks) {
		s.networks = append(s.networks[:idx], s.networks[idx+1:]...)
		// Re-number priorities
		for i := range s.networks {
			s.networks[i].Priority = i + 1
		}
	}
	s.mu.Unlock()

	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}

// handleSave writes config.json + wpa_supplicant.conf, redirects to /rebooting,
// then triggers a system reboot.
func (s *setupServer) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	s.mu.Lock()
	nets := make([]config.WiFiNetCfg, len(s.networks))
	copy(nets, s.networks)
	s.mu.Unlock()

	if len(nets) == 0 {
		http.Redirect(w, r, "/setup?error=Add+at+least+one+WiFi+network+before+saving", http.StatusSeeOther)
		return
	}

	// Update config: save networks, clear force_ap_mode so next boot tries client mode.
	s.cfg.WiFi.Networks = nets
	s.cfg.WiFi.ForceAPMode = false
	s.cfg.WiFi.Mode = "client"

	if err := config.Save(s.configPath, s.cfg); err != nil {
		s.logger.Printf("Setup mode: failed to save config: %v", err)
		http.Redirect(w, r, "/setup?error=Failed+to+save+config:+"+err.Error(), http.StatusSeeOther)
		return
	}
	s.logger.Printf("Setup mode: saved %d network(s) to %s", len(nets), s.configPath)

	// Write wpa_supplicant.conf from the new network list.
	if err := writeWPAConf(nets); err != nil {
		s.logger.Printf("Setup mode: failed to write wpa_supplicant.conf: %v", err)
		// Non-fatal: the gateway will regenerate it on next run if possible.
	} else {
		s.logger.Printf("Setup mode: wrote /data/misc/wifi/wpa_supplicant.conf")
	}

	// Redirect first, then reboot after a short delay so the response is delivered.
	http.Redirect(w, r, "/rebooting", http.StatusSeeOther)

	go func() {
		time.Sleep(500 * time.Millisecond)
		s.logger.Printf("Setup mode: triggering reboot")
		if err := exec.Command("/system/xbin/librank", "/system/bin/reboot").Run(); err != nil {
			s.logger.Printf("Setup mode: reboot command failed: %v", err)
		}
		// If reboot command fails (e.g. dev environment), just wait.
		time.Sleep(60 * time.Second)
	}()
}

// handleRebooting shows a "rebooting" page. The device will reboot within seconds.
func (s *setupServer) handleRebooting(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, rebootingHTML)
}

// writeWPAConf generates /data/misc/wifi/wpa_supplicant.conf from the network list.
func writeWPAConf(networks []config.WiFiNetCfg) error {
	var sb strings.Builder
	sb.WriteString("ctrl_interface=/data/misc/wifi/sockets\nupdate_config=1\nap_scan=1\n")
	for _, n := range networks {
		sb.WriteString("network={\n")
		sb.WriteString(fmt.Sprintf("    ssid=%q\n", n.SSID))
		if strings.ToUpper(n.Security) == "OPEN" || n.Password == "" {
			sb.WriteString("    key_mgmt=NONE\n")
		} else {
			sb.WriteString(fmt.Sprintf("    psk=%q\n", n.Password))
			sb.WriteString("    key_mgmt=WPA-PSK\n")
		}
		sb.WriteString(fmt.Sprintf("    priority=%d\n", n.Priority))
		sb.WriteString("}\n")
	}
	return os.WriteFile("/data/misc/wifi/wpa_supplicant.conf", []byte(sb.String()), 0644)
}

const setupHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>WiFi Setup — SMS Gateway</title>
<link rel="stylesheet" href="/static/pico.min.css">
<link rel="icon" href="/static/logo.png">
<style>
  body { background: #1a1a2e; min-height: 100vh; }
  main { max-width: 480px; margin: 0 auto; padding: 1.5rem 1rem; }
  .header { text-align: center; margin-bottom: 1.5rem; }
  .header img { height: 56px; border-radius: 10px; margin-bottom: .5rem; }
  .header h1 { font-size: 1.3rem; margin: 0; color: #fff; }
  .header p { color: #9ca3af; font-size: .85rem; margin: .25rem 0 0; }
  .card { background: #fff; border-radius: 12px; padding: 1.25rem; margin-bottom: 1rem; }
  .card h2 { font-size: 1rem; margin: 0 0 .75rem; color: #1a1a2e; border-bottom: 1px solid #e5e7eb; padding-bottom: .5rem; }
  .network-item { display: flex; align-items: center; justify-content: space-between;
                  padding: .5rem 0; border-bottom: 1px solid #f3f4f6; }
  .network-item:last-child { border-bottom: none; }
  .network-name { font-size: .95rem; font-weight: 500; }
  .network-meta { font-size: .75rem; color: #9ca3af; }
  .btn-remove { background: none; border: 1px solid #dc3545; color: #dc3545;
                padding: .2rem .6rem; border-radius: 6px; font-size: .8rem; cursor: pointer; }
  .btn-remove:hover { background: #dc3545; color: #fff; }
  .empty { color: #9ca3af; font-size: .9rem; font-style: italic; }
  label { font-size: .9rem; font-weight: 500; color: #374151; display: block; margin-bottom: .25rem; }
  input[type=text], input[type=password], select {
    width: 100%; padding: .55rem .75rem; border: 1px solid #d1d5db; border-radius: 8px;
    font-size: .95rem; margin-bottom: .75rem; box-sizing: border-box; }
  .btn-add { background: #0d6efd; color: #fff; border: none; padding: .6rem 1.25rem;
             border-radius: 8px; font-size: .95rem; font-weight: 600; cursor: pointer; width: 100%; }
  .btn-add:hover { background: #0b5ed7; }
  .btn-save { background: #198754; color: #fff; border: none; padding: .75rem;
              border-radius: 8px; font-size: 1rem; font-weight: 700; cursor: pointer; width: 100%; }
  .btn-save:hover { background: #157347; }
  .error { background: #fef2f2; border: 1px solid #fca5a5; color: #dc2626;
           border-radius: 8px; padding: .6rem .9rem; font-size: .85rem; margin-bottom: .75rem; }
  .hint { color: #6b7280; font-size: .8rem; margin-bottom: 1rem; }
</style>
</head>
<body>
<main>
  <div class="header">
    <img src="/static/logo.png" alt="Marlow FM">
    <h1>SMS Gateway WiFi Setup</h1>
    <p>The gateway could not connect to a known WiFi network.</p>
  </div>

  {{if .Error}}
  <div class="error">{{.Error}}</div>
  {{end}}

  <div class="card">
    <h2>Saved Networks</h2>
    {{if .Networks}}
      {{range $i, $n := .Networks}}
      <div class="network-item">
        <div>
          <div class="network-name">{{$n.SSID}}</div>
          <div class="network-meta">{{$n.Security}} &bull; priority {{$n.Priority}}</div>
        </div>
        <form method="POST" action="/setup/delete" style="margin:0">
          <input type="hidden" name="index" value="{{$i}}">
          <button class="btn-remove" type="submit">Remove</button>
        </form>
      </div>
      {{end}}
    {{else}}
      <p class="empty">No networks saved yet. Add one below.</p>
    {{end}}
  </div>

  <div class="card">
    <h2>Add Network</h2>
    <form method="POST" action="/setup/add">
      <label for="ssid">WiFi Name (SSID)</label>
      <input type="text" id="ssid" name="ssid" placeholder="My WiFi Network" required>

      <label for="psk">Password</label>
      <input type="password" id="psk" name="psk" placeholder="At least 8 characters">

      <label for="security">Security</label>
      <select id="security" name="security">
        <option value="WPA2">WPA2 (recommended)</option>
        <option value="WPA">WPA</option>
        <option value="OPEN">Open (no password)</option>
      </select>

      <button class="btn-add" type="submit">Add Network</button>
    </form>
  </div>

  <div class="card">
    <p class="hint">When you save, the gateway will reboot and connect to one of the networks above.
    This takes about 2 minutes. Your phone will disconnect from SMS-Gateway-Setup when the
    gateway reboots.</p>
    <form method="POST" action="/setup/save">
      <button class="btn-save" type="submit">Save &amp; Reboot</button>
    </form>
  </div>
</main>
</body>
</html>`

const rebootingHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Rebooting — SMS Gateway</title>
<style>
  * { margin:0; padding:0; box-sizing:border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
         background:#1a1a2e; display:flex; align-items:center; justify-content:center;
         min-height:100vh; }
  .card { background:#fff; border-radius:12px; box-shadow:0 2px 16px rgba(0,0,0,.15);
          padding:48px 32px; text-align:center; max-width:380px; width:90%; }
  .card img { height:56px; border-radius:10px; margin-bottom:1rem; }
  .spinner { width:48px; height:48px; border:4px solid #e5e7eb; border-top-color:#1a1a2e;
             border-radius:50%; animation:spin 0.8s linear infinite; margin:0 auto 20px; }
  @keyframes spin { to { transform:rotate(360deg); } }
  h2 { color:#1a1a2e; font-size:1.2rem; margin-bottom:8px; }
  p { color:#6b7280; font-size:.9rem; line-height:1.5; }
  .step { margin-top:16px; font-size:.8rem; color:#9ca3af; }
</style>
</head>
<body>
<div class="card">
  <img src="/static/logo.png" alt="Marlow FM">
  <div class="spinner"></div>
  <h2>WiFi Config Saved</h2>
  <p>The gateway is rebooting and will attempt to connect to your WiFi.</p>
  <p class="step">This takes about 2 minutes.<br>You can disconnect from SMS-Gateway-Setup.</p>
</div>
</body>
</html>`
