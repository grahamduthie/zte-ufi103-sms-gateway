#!/system/bin/sh
# Standalone SMS test - uses fresh SMD channel
DEV=/dev/smd11
BB=/system/bin/busybox
NUMBER="+447700000001"
TEXT="Standalone test $(date +%H:%M:%S)"
LOG=/data/sms-gateway/_test_send.log

echo "=== SMS Send Test $(date) ===" > $LOG
echo "Number: $NUMBER" >> $LOG
echo "Text: $TEXT" >> $LOG

exec 3<>$DEV

# Set text mode
echo "Sending AT+CMGF=1" >> $LOG
$BB printf 'AT+CMGF=1\r\n' >&3
sleep 1
echo "Reading CMGF response:" >> $LOG
$BB dd if=$DEV bs=512 count=1 2>/dev/null >> $LOG

# Set character set
echo "Sending AT+CSCS" >> $LOG
$BB printf 'AT+CSCS="IRA"\r\n' >&3
sleep 1
echo "Reading CSCS response:" >> $LOG
$BB dd if=$DEV bs=512 count=1 2>/dev/null >> $LOG

# Send number
echo "Sending AT+CMGS" >> $LOG
$BB printf "AT+CMGS=\"$NUMBER\"\r" >&3
sleep 3
echo "Reading CMGS response (should see > prompt):" >> $LOG
$BB dd if=$DEV bs=256 count=1 2>/dev/null >> $LOG

# Send message + Ctrl-Z
echo "Sending message text + Ctrl-Z" >> $LOG
$BB printf '%s' "$TEXT" >&3
$BB printf '\x1a' >&3
sleep 8

# Capture response
echo "Reading final response:" >> $LOG
$BB dd if=$DEV bs=4096 count=1 2>/dev/null >> $LOG

echo "=== End ===" >> $LOG
exec 3>&-

cat $LOG
