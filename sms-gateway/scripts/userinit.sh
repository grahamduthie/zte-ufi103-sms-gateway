#!/system/bin/sh
# /data/local/userinit.sh — executed by Android init on boot (if supported).
# Starts the SMS gateway via librank (permanent SUID rootshell).

/system/xbin/librank /system/bin/sh /data/sms-gateway/start.sh &
