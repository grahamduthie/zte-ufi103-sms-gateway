#!/system/bin/sh
# Launch sms-gateway as a true daemon (survives shell exit)
LOG=/data/sms-gateway/sms-gateway.log
PIDFILE=/data/sms-gateway/sms-gateway.pid

# Kill existing
if [ -f "$PIDFILE" ]; then
    kill $(cat $PIDFILE) 2>/dev/null
    kill -9 $(cat $PIDFILE) 2>/dev/null
    rm -f $PIDFILE
fi

# Fork into background and detach
cd /data/sms-gateway
./sms-gateway >> $LOG 2>&1 &
DAEMON_PID=$!
echo $DAEMON_PID > $PIDFILE

# Wait and verify
sleep 3
if kill -0 $DAEMON_PID 2>/dev/null; then
    echo "sms-gateway started (PID $DAEMON_PID)"
else
    echo "sms-gateway FAILED to start"
    cat $LOG
fi
