#!/system/bin/sh
cd /data/sms-gateway
./sms-gateway >> sms-gateway.log 2>&1 &
sleep 3
echo "PID: $!"
ls -la /proc/$! 2>/dev/null
netstat -tlnp 2>/dev/null | grep ':80 '
