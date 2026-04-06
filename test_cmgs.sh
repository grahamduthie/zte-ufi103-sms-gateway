#!/system/bin/busybox sh
exec 3<>/dev/smd11
busybox printf 'AT+CMGF=1\r\n' >&3
sleep 2
# Read from a NEW open of /dev/smd11, not from fd 3
RESP1=$(busybox dd if=/dev/smd11 bs=512 count=1 2>/dev/null)
echo "RESP1 after 2s: [$RESP1]"
i=0
while [ $i -lt 3 ]; do
  sleep 1
  CHUNK=$(busybox dd if=/dev/smd11 bs=512 count=1 2>/dev/null)
  RESP1="$RESP1$CHUNK"
  echo "RESP1 after loop $i: [$RESP1]"
  case "$RESP1" in
  *OK*) break ;;
  esac
  i=$((i+1))
done
echo "=== RESP1 FINAL: [$RESP1] ==="
echo "---"
busybox printf 'AT+CMGS="+447700000001"\r' >&3
sleep 3
RESP2=$(busybox dd if=/dev/smd11 bs=512 count=1 2>/dev/null)
echo "RESP2 after 3s: [$RESP2]"
i=0
while [ $i -lt 5 ]; do
  sleep 1
  CHUNK=$(busybox dd if=/dev/smd11 bs=512 count=1 2>/dev/null)
  RESP2="$RESP2$CHUNK"
  echo "RESP2 after loop $i: [$RESP2]"
  case "$RESP2" in
  *"> "*) break ;;
  esac
  i=$((i+1))
done
echo "=== RESP2 FINAL: [$RESP2] ==="
exec 3>&-
