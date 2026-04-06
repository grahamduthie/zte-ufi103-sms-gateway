package atcmd

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SMS represents a text message from the SIM or modem storage.
type SMS struct {
	Index     int
	Status    string
	Sender    string
	Timestamp string
	Text      string
	Storage   string
}

// SignalInfo holds the current signal strength.
type SignalInfo struct {
	RSSI int
	DBM  int
	Bars int
}

// NetworkInfo holds registration and operator details.
type NetworkInfo struct {
	Registered bool
	Roaming    bool
	Operator   string
	IMSI       string
}

// Session manages AT commands via /dev/smd11 using a persistent file
// descriptor and a background reader goroutine.
//
// Rationale: RILD (PID 192) owns the SMD channel endpoint at the kernel/
// driver level. When we open /dev/smd11 per-command, RILD reads all AT
// responses before we can. A persistent fd with a continuous background
// reader captures everything, and we match responses to commands by
// position in the accumulated buffer.
//
// Buffer management: respBuf is truncated to zero at the start of every
// command (sendCommand, sendCommandsMulti, ensureUnlocked, sendSMSDirectAT).
// Because s.mu serialises all commands, no other command can hold a position
// into the buffer while we truncate. This keeps peak memory bounded to one
// command's worth of modem output (~a few KB) instead of growing forever.
//
// Reader design: readerLoop reads one byte at a time directly from s.file
// rather than via a bufio.Reader. This means that if s.file is ever replaced
// (e.g., after a reopen), the next Read() call picks up the new fd without
// the goroutine needing to be restarted.
//
// +CMTI detection: the readerLoop also monitors unsolicited "+CMTI:" lines
// (new-message-stored indications) and signals NewMessageCh so the SMS poller
// can do an immediate read rather than waiting for the next 2-second tick.
// This dramatically shrinks the window in which RILD can read and delete a
// message before our gateway polls.
type Session struct {
	mu sync.Mutex // serialises all AT command exchanges

	devPath string
	simPIN  string       // optional; used by ensureUnlocked to auto-recover SIM lock
	fdMu    sync.RWMutex // protects s.file pointer for safe future reopen
	file    *os.File

	// respBuf accumulates all lines read from the modem. Truncated to zero at
	// the start of each command sequence to prevent unbounded growth.
	respMu  sync.Mutex
	respBuf []byte

	// readerDone is closed by the reader goroutine when it exits.
	readerDone chan struct{}
	// closeSig is closed by Close() to signal the reader goroutine to stop.
	closeSig chan struct{}

	// NewMessageCh receives a signal whenever the readerLoop sees a +CMTI:
	// unsolicited result code, indicating that a new SMS has been stored on
	// the SIM or modem. The SMS poller selects on this channel in addition to
	// its normal ticker so it can react within milliseconds rather than up to
	// 2 seconds later.
	// Buffered to 1: if a signal is already pending, additional +CMTI: lines
	// are dropped (the pending signal already covers them).
	NewMessageCh chan struct{}

	// promptCh receives a signal the instant readerLoop sees a '>' byte from
	// the modem. The modem sends '>' to signal readiness for PDU input after
	// AT+CMGS. We flush it immediately (without waiting for a newline) and
	// signal here so sendSMSDirectAT can write the PDU with zero polling delay —
	// critical for beating RILD's AT+CPMS injection which arrives ~2ms later.
	// Buffered to 1: if already signalled (e.g. from a previous attempt), the
	// extra signal is dropped.
	promptCh chan struct{}

	// cacheMu protects the signal and network caches.
	cacheMu       sync.RWMutex
	cachedSignal  SignalInfo
	cachedNetwork NetworkInfo
}

// NewSession creates a Session and opens the persistent fd + starts the
// background reader.
func NewSession(path string) (*Session, error) {
	s := &Session{
		devPath:      path,
		readerDone:   make(chan struct{}),
		closeSig:     make(chan struct{}),
		NewMessageCh: make(chan struct{}, 1),
		promptCh:     make(chan struct{}, 1),
	}
	if err := s.open(); err != nil {
		return nil, err
	}
	go s.readerLoop()
	return s, nil
}

// open opens /dev/smd11, drains residual data, and issues a soft reset
// sequence to clear any stuck state left by a previous process being killed
// mid-AT-sequence (e.g. mid-CMGS text input).
func (s *Session) open() error {
	f, err := os.OpenFile(s.devPath, os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open %s: %w", s.devPath, err)
	}

	drain := func(d time.Duration) {
		buf := make([]byte, 4096)
		f.SetReadDeadline(time.Now().Add(d))
		for {
			n, _ := f.Read(buf)
			if n == 0 {
				break
			}
		}
		f.SetReadDeadline(time.Time{})
	}

	// Initial drain of any residual RILD data.
	drain(200 * time.Millisecond)

	// Soft reset sequence: ESC cancels any pending text-input mode (e.g.
	// AT+CMGS waiting for Ctrl-Z), followed by a bare CR to terminate any
	// partial command, then AT to confirm the modem is responsive.
	f.Write([]byte{0x1B})       // ESC
	f.WriteString("\r\n")       // terminate partial command
	time.Sleep(300 * time.Millisecond)
	drain(200 * time.Millisecond)

	f.WriteString("AT\r\n")
	time.Sleep(500 * time.Millisecond)
	drain(300 * time.Millisecond)

	s.fdMu.Lock()
	s.file = f
	s.fdMu.Unlock()
	return nil
}

// Close stops the reader and closes the fd.
func (s *Session) Close() error {
	if s.file != nil {
		select {
		case <-s.closeSig:
			// Already closed.
		default:
			close(s.closeSig)
		}
		s.file.Close()
		<-s.readerDone
	}
	return nil
}

// readerLoop continuously reads from the modem fd one byte at a time,
// assembling complete lines into respBuf. It runs for the lifetime of
// the Session.
//
// Reading byte-by-byte (rather than via bufio.Reader) means that if
// s.file is ever replaced by a reopen, the next Read() call automatically
// uses the new fd — no goroutine restart required.
//
// A 500ms read deadline is set on each call so that closeSig is checked
// at least twice per second without needing a separate select loop.
func (s *Session) readerLoop() {
	defer close(s.readerDone)
	var line []byte
	oneByte := make([]byte, 1)

	for {
		select {
		case <-s.closeSig:
			return
		default:
		}

		// Re-read s.file on each iteration so a future reopen is picked up.
		s.fdMu.RLock()
		f := s.file
		s.fdMu.RUnlock()

		// Short read deadline so we periodically check closeSig.
		f.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := f.Read(oneByte)
		f.SetReadDeadline(time.Time{})

		if err != nil {
			if isTimeoutError(err) {
				continue
			}
			// Non-timeout error (fd closed, device reset, etc.).
			// Check for shutdown, then back off and retry.
			select {
			case <-s.closeSig:
				return
			default:
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}

		if n == 0 {
			continue
		}

		if oneByte[0] == '\n' {
			line = append(line, '\n')
			s.respMu.Lock()
			s.respBuf = append(s.respBuf, line...)
			s.respMu.Unlock()

			// Detect +CMTI: (new SMS stored) and signal the poller immediately.
			// Non-blocking send: if a signal is already pending, drop this one —
			// the pending signal covers it.
			if strings.HasPrefix(strings.TrimSpace(string(line)), "+CMTI:") {
				select {
				case s.NewMessageCh <- struct{}{}:
				default:
				}
			}

			line = line[:0]
		} else {
			line = append(line, oneByte[0])

			// Flush immediately when we see '>': the modem SMS text-input prompt
			// is "\r\n> " — no trailing newline until after the PDU is accepted.
			// Without an immediate flush, '>' never reaches respBuf and
			// sendSMSDirectAT can only detect it when RILD happens to follow it
			// with a newline (i.e., RILD has already injected bytes). By flushing
			// here and signalling promptCh, sendSMSDirectAT detects the prompt
			// within microseconds and can write the PDU before RILD reacts.
			if oneByte[0] == '>' {
				s.respMu.Lock()
				s.respBuf = append(s.respBuf, line...)
				s.respMu.Unlock()
				line = line[:0]
				// Non-blocking signal: if a signal is already pending (e.g. a
				// previous attempt), drop this — the consumer will drain it.
				select {
				case s.promptCh <- struct{}{}:
				default:
				}
			}
		}
	}
}

func isTimeoutError(err error) bool {
	type timeoutErr interface {
		Timeout() bool
	}
	if te, ok := err.(timeoutErr); ok {
		return te.Timeout()
	}
	return false
}

// sendCommand writes an AT command and waits for a terminal response.
// The buffer is truncated before writing, so startPos is always 0.
func (s *Session) sendCommand(cmd string, timeout time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Truncate buffer to prevent unbounded growth. Safe because s.mu
	// serialises all commands — no other goroutine holds a position into it.
	s.respMu.Lock()
	s.respBuf = s.respBuf[:0]
	s.respMu.Unlock()

	if _, err := s.file.WriteString(cmd + "\r\n"); err != nil {
		return "", fmt.Errorf("write %q: %w", cmd, err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		s.respMu.Lock()
		resp := string(s.respBuf)
		s.respMu.Unlock()

		if isTerminalResponse(resp) {
			return resp, nil
		}
	}

	s.respMu.Lock()
	resp := string(s.respBuf)
	s.respMu.Unlock()
	return resp, fmt.Errorf("timed out waiting for response to %q, got: %q", cmd, truncateStr(resp, 300))
}

// sendCommandRaw writes an AT command and returns ALL accumulated
// responses up to the timeout. Used for multi-command sequences
// where we want the complete output.
func (s *Session) sendCommandRaw(cmd string, timeout time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.respMu.Lock()
	s.respBuf = s.respBuf[:0]
	s.respMu.Unlock()

	if _, err := s.file.WriteString(cmd + "\r\n"); err != nil {
		return "", fmt.Errorf("write %q: %w", cmd, err)
	}

	time.Sleep(timeout)

	s.respMu.Lock()
	resp := string(s.respBuf)
	s.respMu.Unlock()
	return resp, nil
}

// sendCommandsMulti writes multiple commands and returns all responses
// concatenated. Used by ListSMS, GetSMSCount, etc.
func (s *Session) sendCommandsMulti(cmds []string, perCmdWait time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Truncate once for the whole sequence.
	s.respMu.Lock()
	s.respBuf = s.respBuf[:0]
	s.respMu.Unlock()

	for _, cmd := range cmds {
		if _, err := s.file.WriteString(cmd + "\r\n"); err != nil {
			s.respMu.Lock()
			resp := string(s.respBuf)
			s.respMu.Unlock()
			return resp, fmt.Errorf("write %q: %w", cmd, err)
		}
		time.Sleep(perCmdWait)
	}

	// Wait a bit for the last command's response to arrive.
	time.Sleep(500 * time.Millisecond)

	s.respMu.Lock()
	resp := string(s.respBuf)
	s.respMu.Unlock()
	return resp, nil
}

func isTerminalResponse(resp string) bool {
	lines := strings.Split(resp, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "OK" || line == "ERROR" ||
			strings.HasPrefix(line, "+CMGS:") ||
			strings.HasPrefix(line, "+CMS ERROR:") ||
			strings.HasPrefix(line, "+CME ERROR:") {
			return true
		}
	}
	return false
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// SetSIMPIN stores the SIM PIN so that ensureUnlocked can use it for
// automatic re-lock recovery. Must be called before SendSMS if the SIM
// may be PIN-locked (e.g. after a WiFi mode switch).
func (s *Session) SetSIMPIN(pin string) {
	s.simPIN = pin
}

// RespBufSize returns the current size of the response buffer in bytes.
// Used for health monitoring.
func (s *Session) RespBufSize() int {
	s.respMu.Lock()
	defer s.respMu.Unlock()
	return len(s.respBuf)
}

// SetTextMode configures the modem for text-mode SMS and ensures incoming
// messages are stored in the preferred storage (SM) via AT+CNMI=2,1,0,0,0.
//
// RILD sets AT+CNMI=0,0,0,0,0 at boot (mt=0), which on Qualcomm MSM8916
// devices causes incoming SMS to be routed exclusively via QMI WMS and NOT
// written to AT-accessible SM storage. Setting mt=1 tells the modem to store
// each new SMS in the AT+CPMS preferred storage (SM) and emit a +CMTI
// unsolicited result code — which our readerLoop already handles.
func (s *Session) SetTextMode(storage string) error {
	cmds := []string{
		fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, storage, storage, storage),
		"AT+CMGF=1",
		`AT+CSCS="IRA"`,
		`AT+CNMI=2,1,0,0,0`,
	}
	_, err := s.sendCommandsMulti(cmds, 800*time.Millisecond)
	return err
}

// ListSMS reads all messages from the specified storage.
func (s *Session) ListSMS(storage string) ([]SMS, error) {
	cmds := []string{
		fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, storage, storage, storage),
		"AT+CMGF=1",
		`AT+CSCS="IRA"`,
		`AT+CMGL="ALL"`,
	}
	out, err := s.sendCommandsMulti(cmds, 800*time.Millisecond)
	if err != nil {
		// Partial response — still try to parse.
	}
	return parseCMGL(out, storage)
}

// GetSMSCount returns the number of messages in the specified storage.
func (s *Session) GetSMSCount(storage string) (int, error) {
	cmds := []string{
		fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, storage, storage, storage),
		"AT+CMGF=1",
		`AT+CSCS="IRA"`,
		// AT+CNMI is intentionally NOT sent here. Sending it every 2s causes
		// RILD to react with AT+CPMS="SM","SM","SM" on every cycle, which
		// injects into the SMS send window and corrupts outbound messages.
		// AT+CNMI=2,1,0,0,0 is applied once at startup via SetTextMode().
		"AT+CPMS?",
	}
	out, err := s.sendCommandsMulti(cmds, 600*time.Millisecond)
	if err != nil {
		return 0, err
	}
	// CRITICAL: RILD also sends AT+CPMS? every 3-5s on the shared /dev/smd11
	// fd. RILD's response (which may show count=0 after RILD reads SMS via QMI)
	// can appear in our buffer AFTER our own response. We must find the CPMS
	// line that comes IMMEDIATELY after OUR "AT+CPMS?" command, not just any
	// CPMS line.
	pattern := "AT+CPMS?\r\n"
	idx := strings.LastIndex(out, pattern)
	if idx == -1 {
		// Try without \r\n in case of line ending variations
		idx = strings.LastIndex(out, "AT+CPMS?")
	}
	if idx >= 0 {
		out = out[idx+len(pattern):]
	}
	re := regexp.MustCompile(`\+CPMS:\s*"` + regexp.QuoteMeta(storage) + `",(\d+)`)
	matches := re.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return 0, fmt.Errorf("cannot parse CPMS response: %s", out)
	}
	return strconv.Atoi(matches[0][1])
}

// ReadSMS reads a single message by its SIM index.
func (s *Session) ReadSMS(index int, storage string) (*SMS, error) {
	cmds := []string{
		fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, storage, storage, storage),
		"AT+CMGF=1",
		`AT+CSCS="IRA"`,
		fmt.Sprintf("AT+CMGR=%d", index),
	}
	out, err := s.sendCommandsMulti(cmds, 800*time.Millisecond)
	if err != nil {
		return nil, err
	}
	msgs, err := parseCMGL(out, storage)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	return &msgs[0], nil
}

// DeleteSMS deletes a message by index.
func (s *Session) DeleteSMS(index int) error {
	out, err := s.sendCommand(fmt.Sprintf("AT+CMGD=%d,0", index), 3*time.Second)
	if err != nil {
		return err
	}
	if strings.Contains(out, "ERROR") {
		return fmt.Errorf("delete failed: %s", strings.TrimSpace(out))
	}
	return nil
}

// SendSMS sends an SMS via AT+CMGS on the persistent /dev/smd11 fd.
func (s *Session) SendSMS(number, text string) (int, error) {
	// Ensure SIM is unlocked (after modem reset, SIM may be PIN-locked).
	if err := s.ensureUnlocked(); err != nil {
		return 0, fmt.Errorf("SIM unlock: %w", err)
	}
	return s.sendSMSDirectAT(number, text)
}

// EnsureUnlocked checks if the SIM is PIN-locked and unlocks it if needed.
// Safe to call speculatively — if the SIM is already ready it returns immediately.
func (s *Session) EnsureUnlocked() error {
	return s.ensureUnlocked()
}

// ensureUnlocked is the internal implementation.
func (s *Session) ensureUnlocked() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// ESC to cancel any pending text-input mode before sending AT+CPIN?.
	// If the modem is stuck in text mode, AT+CPIN? would be buffered as SMS
	// body text rather than executed as an AT command.
	s.file.Write([]byte{0x1B})
	time.Sleep(100 * time.Millisecond)

	// Truncate at start of this command sequence.
	s.respMu.Lock()
	s.respBuf = s.respBuf[:0]
	s.respMu.Unlock()

	s.file.WriteString("AT+CPIN?\r\n")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		s.respMu.Lock()
		portion := string(s.respBuf)
		s.respMu.Unlock()

		if strings.Contains(portion, "+CPIN: READY") {
			return nil
		}
		if strings.Contains(portion, "+CPIN: SIM PIN") {
			if s.simPIN == "" {
				return fmt.Errorf("SIM is PIN-locked but no PIN is configured")
			}
			// Record position before sending PIN so we don't match old READY.
			s.respMu.Lock()
			pinStartPos := len(s.respBuf)
			s.respMu.Unlock()
			s.file.WriteString(fmt.Sprintf("AT+CPIN=\"%s\"\r\n", s.simPIN))
			if err := s.waitForAfter("OK", pinStartPos, 5*time.Second); err != nil {
				return fmt.Errorf("PIN unlock failed: %w", err)
			}
			// Wait for SIM to initialise after unlock.
			time.Sleep(5 * time.Second)
			return nil
		}
		if strings.Contains(portion, "ERROR") {
			// AT+CPIN? returned ERROR — this usually means RILD's AT+CPMS?
			// poll interleaved our query. The SIM is NOT asking for a PIN
			// (that would be "+CPIN: SIM PIN", not "ERROR"). So we can
			// safely assume the SIM is not PIN-locked and return success.
			return nil
		}
	}
	// Timeout with no definitive answer — return nil to avoid blocking the
	// poll. GetSMSCount will be the real indicator of SIM health. If the SIM
	// is genuinely PIN-locked, GetSMSCount will fail with an error.
	return nil
}

// sendSMSDirectAT sends SMS via AT+CMGS in PDU mode (AT+CMGF=0).
//
// PDU mode is used rather than text mode (AT+CMGF=1) specifically to handle
// the RILD injection problem: RILD polls /dev/smd11 every 3-5s with AT+CPMS?
// While the modem is in text-input mode, any bytes it receives are buffered as
// SMS body text — including RILD's AT commands. In text mode this silently
// corrupts the SMS. In PDU mode the modem expects a strict hex string; RILD's
// injected bytes contain non-hex characters, causing the modem to return
// +CMS ERROR instead of sending a garbled SMS. We get a clean failure and retry.
//
// The inner prompt-retry loop handles the second RILD problem: RILD's reader on
// /dev/smd11 sometimes consumes the '>' prompt before our readerLoop can read
// it. Up to maxPromptTries quick retries avoids a 5-minute queue backoff.
func (s *Session) sendSMSDirectAT(number, text string) (int, error) {
	if number == "" {
		return 0, fmt.Errorf("phone number is empty")
	}
	if text == "" {
		return 0, fmt.Errorf("SMS text is empty")
	}
	if len([]rune(text)) > 160 {
		return 0, fmt.Errorf("SMS text too long: %d chars (max 160)", len([]rune(text)))
	}

	// Build the PDU before taking the mutex — pure computation, no I/O.
	// tpduHex is the TP layer PDU as uppercase hex (no SMSC prefix).
	// We prepend "00" when sending to tell the modem to use the SIM's stored SMSC.
	tpduHex, err := encodeSMSPDU(number, text)
	if err != nil {
		return 0, fmt.Errorf("PDU encode: %w", err)
	}
	tpduLen := len(tpduHex) / 2      // TPDU length in octets (for AT+CMGS=<n>)
	fullPDU := "00" + tpduHex        // SMSC len=0 means "use SIM default SMSC"
	cmgsCmd := fmt.Sprintf("AT+CMGS=%d\r", tpduLen)

	s.mu.Lock()
	defer s.mu.Unlock()

	// ESC to cancel any text-input mode left over from a previous failed send.
	// Without this, AT+CMGF=0 below would be treated as SMS body text.
	s.file.Write([]byte{0x1B})
	time.Sleep(100 * time.Millisecond)

	s.respMu.Lock()
	s.respBuf = s.respBuf[:0]
	s.respMu.Unlock()

	// Step 1: Set PDU mode.
	// RILD reacts to AT+CMGF=0 by immediately sending AT+CPMS="SM","SM","SM"
	// to reclaim its preferred storage setting. If we send AT+CMGS too quickly,
	// RILD's CPMS bytes arrive while the modem is in text-input mode and corrupt
	// the PDU. We wait for the channel to go quiet (no new bytes for quietPeriod)
	// before proceeding — this lets RILD's reaction flush completely.
	s.file.WriteString("AT+CMGF=0\r\n")

	const quietPeriod = 250 * time.Millisecond
	const maxQuietWait = 3 * time.Second
	quietStart := time.Now()
	lastBufLen := -1
	for time.Since(quietStart) < maxQuietWait {
		time.Sleep(quietPeriod)
		s.respMu.Lock()
		currentLen := len(s.respBuf)
		s.respMu.Unlock()
		if currentLen == lastBufLen {
			break // buffer hasn't grown — channel is quiet
		}
		lastBufLen = currentLen
	}

	// Truncate buffer now that RILD's reaction has flushed. This gives us a
	// clean baseline from which to detect the AT+CMGS prompt and response.
	s.respMu.Lock()
	s.respBuf = s.respBuf[:0]
	s.respMu.Unlock()

	// Drain any stale signal in promptCh from a previous attempt.
	select {
	case <-s.promptCh:
	default:
	}

	// Step 2: Issue AT+CMGS=<tpduLen> and wait for the '>' prompt, then send
	// the PDU immediately. The readerLoop flushes '>' to respBuf without
	// waiting for a newline and signals promptCh the instant it arrives —
	// this lets us write the PDU in microseconds, beating RILD's AT+CPMS
	// injection (~2ms after modem sends '>').
	//
	// Up to maxPromptTries attempts handle transient errors.
	const maxPromptTries = 5
	sentPDU := false
	var lastPortion string

	for try := 0; try < maxPromptTries && !sentPDU; try++ {
		if try > 0 {
			// Drain stale signal before retry.
			select {
			case <-s.promptCh:
			default:
			}
			time.Sleep(200 * time.Millisecond)
		}

		s.respMu.Lock()
		cmgsStart := len(s.respBuf)
		s.respMu.Unlock()

		s.file.WriteString(cmgsCmd)

		// Wait for '>' prompt via promptCh (signalled by readerLoop immediately
		// when it sees '>') or fall back to polling the buffer for up to 5s.
		promptDeadline := time.NewTimer(5 * time.Second)
		promptReceived := false
		for !promptReceived {
			select {
			case <-s.promptCh:
				// Prompt received — check buffer for errors before sending.
				s.respMu.Lock()
				portion := ""
				if cmgsStart < len(s.respBuf) {
					portion = string(s.respBuf[cmgsStart:])
				}
				s.respMu.Unlock()
				lastPortion = portion
				if strings.Contains(portion, "+CMS ERROR") ||
					strings.Contains(portion, "+CME ERROR") ||
					strings.Contains(portion, "\nERROR\r") {
					promptDeadline.Stop()
					s.file.Write([]byte{0x1B})
					time.Sleep(100 * time.Millisecond)
					return 0, fmt.Errorf("modem rejected AT+CMGS: %q", truncateStr(portion, 200))
				}
				// '>' confirmed in buffer — send PDU + Ctrl-Z immediately.
				s.file.Write(append([]byte(fullPDU), 0x1A))
				sentPDU = true
				promptReceived = true

			case <-promptDeadline.C:
				// Timed out — check if '>' arrived via normal buffer polling
				// (fallback for any case where promptCh was missed).
				s.respMu.Lock()
				portion := ""
				if cmgsStart < len(s.respBuf) {
					portion = string(s.respBuf[cmgsStart:])
				}
				s.respMu.Unlock()
				lastPortion = portion
				if strings.Contains(portion, ">") {
					s.file.Write(append([]byte(fullPDU), 0x1A))
					sentPDU = true
				}
				promptReceived = true // exit inner loop regardless
			}
		}
		promptDeadline.Stop()

		if !sentPDU {
			// Timed out: modem may be in text-input mode waiting for content.
			// ESC cancels it before we retry.
			s.file.Write([]byte{0x1B})
			time.Sleep(100 * time.Millisecond)
		}
	}

	if !sentPDU {
		return 0, fmt.Errorf("no '>' prompt after %d AT+CMGS tries, last buffer: %q",
			maxPromptTries, truncateStr(lastPortion, 300))
	}

	// Step 3: Wait for +CMGS confirmation (up to 35s).
	// If RILD injected bytes into the PDU stream, the modem will return
	// +CMS ERROR instead of +CMGS — a clean failure, no garbled SMS was sent.
	// Some modem firmware returns a bare "OK" without +CMGS: <ref>; treat that
	// as success with ref=0 rather than spinning until timeout.
	confirmDeadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(confirmDeadline) {
		time.Sleep(500 * time.Millisecond)
		s.respMu.Lock()
		portion := string(s.respBuf)
		s.respMu.Unlock()

		if strings.Contains(portion, "+CMGS:") {
			re := regexp.MustCompile(`\+CMGS:\s*(\d+)`)
			if m := re.FindStringSubmatch(portion); m != nil {
				ref, _ := strconv.Atoi(m[1])
				return ref, nil
			}
		}
		// Bare "OK" without +CMGS: ref — some firmware omits the reference.
		// Only accept this after PDU was sent (sentPDU=true, which it is here).
		if strings.Contains(portion, "\r\nOK\r\n") {
			return 0, nil
		}
		if strings.Contains(portion, "+CMS ERROR") ||
			strings.Contains(portion, "+CME ERROR") {
			s.file.Write([]byte{0x1B})
			time.Sleep(100 * time.Millisecond)
			return 0, fmt.Errorf("modem error after PDU send (RILD injection likely): %q",
				truncateStr(portion, 300))
		}
	}

	s.file.Write([]byte{0x1B})
	time.Sleep(100 * time.Millisecond)
	s.respMu.Lock()
	portion := string(s.respBuf)
	s.respMu.Unlock()
	if portion != "" {
		return 0, fmt.Errorf("no +CMGS response within 35s, got: %q", truncateStr(portion, 300))
	}
	return 0, fmt.Errorf("no +CMGS response within 35s")
}

// waitForAfter waits until the response buffer (from the given position)
// contains the target string.
func (s *Session) waitForAfter(target string, afterPos int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		s.respMu.Lock()
		buf := s.respBuf
		var portion string
		if afterPos < len(buf) {
			portion = string(buf[afterPos:])
		}
		s.respMu.Unlock()

		if strings.Contains(portion, target) {
			return nil
		}
	}
	s.respMu.Lock()
	all := string(s.respBuf)
	s.respMu.Unlock()
	safeStart := afterPos - 20
	if safeStart < 0 {
		safeStart = 0
	}
	if afterPos <= len(all) {
		return fmt.Errorf("timed out waiting for %q, buffer: %q",
			target, truncateStr(all[safeStart:], 400))
	}
	return fmt.Errorf("timed out waiting for %q", target)
}

// SendRaw sends a single AT command and returns the raw response.
// Used by --test-diag and other diagnostic tooling.
func (s *Session) SendRaw(cmd string, timeout time.Duration) (string, error) {
	return s.sendCommand(cmd, timeout)
}

// SendRawMulti sends multiple AT commands and returns all output.
func (s *Session) SendRawMulti(cmds []string, perCmdWait time.Duration) (string, error) {
	return s.sendCommandsMulti(cmds, perCmdWait)
}

// GetSignal returns signal strength info and updates the cache.
func (s *Session) GetSignal() (SignalInfo, error) {
	out, err := s.sendCommand("AT+CSQ", 3*time.Second)
	if err != nil {
		return SignalInfo{}, err
	}
	re := regexp.MustCompile(`\+CSQ:\s*(\d+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return SignalInfo{}, fmt.Errorf("cannot parse CSQ: %s", out)
	}
	rssi, _ := strconv.Atoi(m[1])
	info := SignalInfo{RSSI: rssi}
	if rssi < 99 {
		info.DBM = -113 + rssi*2
		info.Bars = rssi / 6
		if info.Bars > 5 {
			info.Bars = 5
		}
	}
	s.cacheMu.Lock()
	s.cachedSignal = info
	s.cacheMu.Unlock()
	return info, nil
}

// CachedSignal returns the last known signal info without taking the AT mutex.
func (s *Session) CachedSignal() SignalInfo {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return s.cachedSignal
}

// GetNetworkInfo returns registration and operator info and updates the cache.
func (s *Session) GetNetworkInfo() (NetworkInfo, error) {
	info := NetworkInfo{}

	out, err := s.sendCommand("AT+CREG?", 3*time.Second)
	if err == nil {
		re := regexp.MustCompile(`\+CREG:\s*(?:\d+,)?(\d)`)
		m := re.FindStringSubmatch(out)
		if m != nil {
			code, _ := strconv.Atoi(m[1])
			info.Registered = code == 1 || code == 5
			info.Roaming = code == 5
		}
	}

	out, err = s.sendCommand("AT+COPS?", 5*time.Second)
	if err == nil {
		// Response: +COPS: 0,0,"spusu spusu",7
		re := regexp.MustCompile(`\+COPS:\s*\d+,\d+,"([^"]+)"`)
		m := re.FindStringSubmatch(out)
		if m != nil {
			info.Operator = m[1]
		}
	}

	out, err = s.sendCommand("AT+CIMI", 3*time.Second)
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if len(line) == 15 && line[0] >= '0' && line[0] <= '9' {
				info.IMSI = line
				break
			}
		}
	}

	s.cacheMu.Lock()
	s.cachedNetwork = info
	s.cacheMu.Unlock()
	return info, nil
}

// CachedNetworkInfo returns the last known network info without taking the AT mutex.
func (s *Session) CachedNetworkInfo() NetworkInfo {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return s.cachedNetwork
}

// UnlockSIM sends the SIM PIN to unlock the modem.
func (s *Session) UnlockSIM(pin string) error {
	resp, err := s.sendCommand(fmt.Sprintf("AT+CPIN=\"%s\"", pin), 5*time.Second)
	if err != nil {
		return err
	}
	if strings.Contains(resp, "ERROR") || strings.Contains(resp, "+CME ERROR") ||
		strings.Contains(resp, "+CMS ERROR") {
		return fmt.Errorf("PIN unlock failed: %s", strings.TrimSpace(resp))
	}
	if !strings.Contains(resp, "OK") {
		return fmt.Errorf("unexpected PIN unlock response: %s", strings.TrimSpace(resp))
	}
	return nil
}

// GetPINStatus returns whether the SIM is PIN-locked.
func (s *Session) GetPINStatus() (bool, error) {
	resp, err := s.sendCommand("AT+CPIN?", 3*time.Second)
	if err != nil {
		return false, err
	}
	return strings.Contains(resp, "SIM PIN"), nil
}

// GetPhoneNumber returns our own number from the SIM (AT+CNUM).
func (s *Session) GetPhoneNumber() (string, error) {
	out, err := s.sendCommand("AT+CNUM", 3*time.Second)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`\+CNUM:\s*"[^"]*","([^"]*)"`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("cannot parse CNUM response: %s", out)
	}
	return m[1], nil
}

// GetSMSC returns the SMSC number (AT+CSCA?).
func (s *Session) GetSMSC() (string, error) {
	out, err := s.sendCommand("AT+CSCA?", 3*time.Second)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`\+CSCA:\s*"([^"]*)"`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("cannot parse CSCA response: %s", out)
	}
	return m[1], nil
}

// parseCMGL parses the output of AT+CMGL="ALL" into SMS structs.
//
// The previous implementation had a double-increment bug (for-loop i++ plus
// a manual i++) that caused messages with empty bodies to skip the following
// message header. This rewrite detects +CMGL: lines explicitly and collects
// all subsequent non-header lines as a multi-line body, advancing i to the
// last body line so the outer loop's i++ lands on the next header.
func parseCMGL(output, storage string) ([]SMS, error) {
	var msgs []SMS
	lines := strings.Split(output, "\n")
	re := regexp.MustCompile(`\+CMGL:\s*(\d+),"([^"]*)","([^"]*)",.*?,"([^"]*)"`)

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "+CMGL:") {
			continue
		}
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		idx, _ := strconv.Atoi(m[1])
		sms := SMS{
			Index:     idx,
			Status:    m[2],
			Sender:    m[3],
			Timestamp: m[4],
			Storage:   storage,
		}

		// Collect subsequent lines as the message body, stopping at the next
		// header, an "OK" terminal, or an empty line.
		var bodyLines []string
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "+CMGL:") || next == "OK" || next == "ERROR" ||
				strings.HasPrefix(next, "+CME ERROR:") || strings.HasPrefix(next, "+CMS ERROR:") {
				break
			}
			bodyLines = append(bodyLines, next)
			i = j // advance outer loop past the body lines we've consumed
		}

		sms.Text = decodeIfNeeded(strings.Join(bodyLines, "\n"))
		msgs = append(msgs, sms)
	}

	return msgs, nil
}

// decodeIfNeeded detects hex-encoded GSM 7-bit text and decodes it.
// After decoding, the output is validated: if any byte is a non-printable
// control character (below 0x20, excluding \n \r \t) the original text is
// returned unchanged. This prevents false-positive decoding of legitimate
// hex strings like "Balance: 00AABB".
func decodeIfNeeded(text string) string {
	if len(text) < 10 || len(text)%2 != 0 {
		return text
	}
	hexChars := "0123456789ABCDEFabcdef"
	for _, c := range text {
		if !strings.ContainsRune(hexChars, c) {
			return text
		}
	}

	data := make([]byte, len(text)/2)
	for i := 0; i < len(data); i++ {
		_, err := fmt.Sscanf(text[i*2:i*2+2], "%02x", &data[i])
		if err != nil {
			return text
		}
	}

	decoded := gsm7Decode(data)

	// Validate: decoded output must be printable ASCII to be accepted.
	for _, r := range decoded {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			return text // non-printable — keep original hex text
		}
	}
	return decoded
}

// gsm7Decode decodes GSM 7-bit packed data to a string.
func gsm7Decode(data []byte) string {
	var result strings.Builder
	shift := 0
	tmp := 0

	for _, b := range data {
		tmp |= int(b) << shift
		shift += 8
		for shift >= 7 {
			c := tmp & 0x7F
			if c == 0x1B {
				result.WriteRune('~')
			} else {
				result.WriteRune(rune(c))
			}
			tmp >>= 7
			shift -= 7
		}
	}
	if shift > 0 && tmp != 0 {
		c := tmp & 0x7F
		if c != 0 {
			result.WriteRune(rune(c))
		}
	}

	return result.String()
}
