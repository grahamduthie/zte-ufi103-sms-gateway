#!/system/bin/busybox sh
exec 3<>/dev/smd11
echo "fd 3 opened"
busybox printf 'AT\r\n' >&3
echo "wrote AT"
# Read byte-by-byte to see exactly what arrives
i=0
while [ $i -lt 20 ]; do
  BYTE=$(busybox dd if=/proc/self/fd/3 bs=1 count=1 2>/dev/null)
  echo "byte $i: [$BYTE]"
  i=$((i+1))
done
exec 3>&-
echo "done"
