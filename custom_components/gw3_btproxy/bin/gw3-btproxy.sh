#!/bin/sh
# Switch the gateway's BT chip between Xiaomi's silabs_ncp_bt and gw3-btproxy.
#   gw3-btproxy.sh on      proxy mode (saved, survives reboots via "status")
#   gw3-btproxy.sh off     Xiaomi mode (saved)
#   gw3-btproxy.sh status  prints on/off; in proxy mode it also starts the proxy if it is not running
#
# daemon_miio.sh restarts silabs_ncp_bt when no process with that name runs, and pulses the chip
# reset GPIO31 every ~7 s while none does. The proxy runs with "-tag silabs_ncp_bt" so the daemon
# leaves it alone; when the proxy exits, the daemon brings Xiaomi's app back by itself.

DIR=/data
BIN=$DIR/gw3-btproxy
MODE_FILE=$DIR/gw3-btproxy.mode

proxy_pid() { ps -ww | grep "$BIN -tag" | grep -v grep | awk '{print $1}'; }
daemon_pid() { ps -ww | grep daemon_miio.sh | grep -v grep | awk '{print $1}'; }

start() {
	[ -n "$(proxy_pid)" ] && return
	D=$(daemon_pid)
	[ -n "$D" ] && kill -STOP $D
	killall silabs_ncp_bt 2>/dev/null
	sleep 2
	echo 1 > /sys/class/gpio/gpio31/value
	(trap "" HUP; $BIN -tag silabs_ncp_bt 2>&1 | logger -t "<BTP>") </dev/null >/dev/null 2>&1 &
	sleep 3
	[ -n "$D" ] && kill -CONT $D
}

stop() {
	P=$(proxy_pid)
	[ -n "$P" ] && kill $P
}

case "$1" in
on)
	[ "$(cat $MODE_FILE 2>/dev/null)" = proxy ] || echo proxy > $MODE_FILE
	start
	;;
off)
	[ "$(cat $MODE_FILE 2>/dev/null)" = xiaomi ] || echo xiaomi > $MODE_FILE
	stop
	;;
status)
	[ "$(cat $MODE_FILE 2>/dev/null)" = proxy ] && start
	;;
*)
	echo "usage: $0 on|off|status" >&2
	exit 1
	;;
esac
[ -n "$(proxy_pid)" ] && echo on || echo off
