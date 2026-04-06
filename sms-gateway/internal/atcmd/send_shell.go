package atcmd

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// sendSMSViaShell sends an SMS by spawning a root shell subprocess
// (via /system/xbin/librank) that opens a fresh /dev/smd11 fd, sends the
// AT+CMGS sequence with proper timing, and reads the response.
//
// DEPRECATED: This function is DEAD CODE — it is never called by the gateway.
// The production SMS send path uses sendSMSDirectAT() which writes directly
// to the persistent /dev/smd11 file descriptor (no shell, no subprocess).
// This function is retained only for diagnostic/testing purposes and MUST NOT
// be used in production. If you find yourself needing to call this, fix the
// real issue instead — the persistent reader is the correct design.
//
// Input validation: phone numbers are validated to contain only digits, '+',
// '-', '(', ')', and spaces. Text is validated to reject shell metacharacters
// (backticks, $(), $(), semicolons, pipes, ampersands) even though the script
// uses single-quote wrapping. Defence in depth.
func sendSMSViaShell(number, text string) (int, error) {
	// Validate phone number — reject anything that looks like shell code.
	// Allowed: digits, +, -, (, ), space. This is stricter than AT+CMGS
	// requires but prevents any possibility of shell injection.
	validNumber := regexp.MustCompile(`^[0-9+\-() ]+$`)
	if !validNumber.MatchString(number) {
		return 0, fmt.Errorf("invalid phone number %q: only digits, +, -, (, ) and spaces allowed", number)
	}

	// Validate text — reject shell metacharacters as defence in depth.
	// The script uses single-quote wrapping so these are already safe,
	// but we reject them anyway to prevent any future regression.
	if strings.ContainsAny(text, "`$;|&\\") {
		return 0, fmt.Errorf("invalid SMS text: contains characters that are not permitted")
	}

	// Escape single quotes in text and number for shell safety.
	safeText := strings.ReplaceAll(text, "'", "'\\''")
	safeNumber := strings.ReplaceAll(number, "'", "'\\''")

	// Key: we read from fd 3 (the same fd we write to) using
	// dd if=/proc/self/fd/3. We accumulate multiple reads in a loop
	// because RILD's QMI traffic echoes onto /dev/smd11 as AT command
	// noise, interleaved with our responses. We keep reading until we
	// find our expected pattern.
	script := fmt.Sprintf(`#!/system/bin/sh
exec 3<>/dev/smd11
busybox printf 'AT+CMGF=1\r\n' >&3
RESP1=""
i=0
while [ $i -lt 10 ]; do
  CHUNK=$(busybox dd if=/proc/self/fd/3 bs=512 count=1 2>/dev/null)
  RESP1="$RESP1$CHUNK"
  case "$RESP1" in
  *OK*) break ;;
  esac
  sleep 1
  i=$((i+1))
done
busybox printf 'AT+CMGS="%s"\r' >&3
RESP2=""
i=0
while [ $i -lt 15 ]; do
  CHUNK=$(busybox dd if=/proc/self/fd/3 bs=512 count=1 2>/dev/null)
  RESP2="$RESP2$CHUNK"
  case "$RESP2" in
  *"> "*) break ;;
  esac
  sleep 1
  i=$((i+1))
done
case "$RESP2" in
*"> "*)
  busybox printf '%%s\032' '%s' >&3
  RESP3=""
  i=0
  while [ $i -lt 35 ]; do
    CHUNK=$(busybox dd if=/proc/self/fd/3 bs=4096 count=1 2>/dev/null)
    RESP3="$RESP3$CHUNK"
    case "$RESP3" in
    *+CMGS:*|*ERROR*) break ;;
    esac
    sleep 1
    i=$((i+1))
  done
  exec 3>&-
  echo "OK:$RESP3"
  ;;
*)
  exec 3>&-
  echo "NO_PROMPT:$RESP2"
  exit 1
  ;;
esac
`, safeNumber, safeText)

	// Write script to a temp file on device.
	tmpScript := "/data/sms-gateway/send_sms.sh"
	if err := os.WriteFile(tmpScript, []byte(script), 0755); err != nil {
		return 0, fmt.Errorf("write temp script: %w", err)
	}

	// Run via librank (SUID root).
	cmd := exec.Command("/system/xbin/librank", "/system/bin/sh", tmpScript)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		return 0, fmt.Errorf("shell SMS send failed (exit %v): %s", err, truncateStr(output, 300))
	}

	if strings.HasPrefix(output, "NO_PROMPT:") {
		return 0, fmt.Errorf("no '> ' prompt after CMGS, got: %s",
			truncateStr(strings.TrimPrefix(output, "NO_PROMPT:"), 200))
	}

	if !strings.HasPrefix(output, "OK:") {
		return 0, fmt.Errorf("unexpected output: %s", truncateStr(output, 200))
	}

	finalResp := strings.TrimPrefix(output, "OK:")

	// Extract +CMGS reference from final response.
	re := regexp.MustCompile(`\+CMGS:\s*(\d+)`)
	m := re.FindStringSubmatch(finalResp)
	if m == nil {
		return 0, fmt.Errorf("no +CMGS in response: %s", truncateStr(finalResp, 200))
	}
	ref, _ := strconv.Atoi(m[1])
	return ref, nil
}
