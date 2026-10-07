#!/bin/sh
# Switch the gateway's BT chip between Xiaomi's silabs_ncp_bt and gw3-btproxy.
#   gw3-btproxy.sh on       proxy mode (saved)
#   gw3-btproxy.sh off      Xiaomi mode (saved)
#   gw3-btproxy.sh restore  in proxy mode, start the proxy if it is not running (the boot hook calls this)
#   gw3-btproxy.sh status   prints on/off. Read-only when the boot hook is installed; without it, it also
#                           restores (so the Home Assistant poll brings the proxy back after a reboot)
#
# daemon_miio.sh restarts silabs_ncp_bt when no process with that name runs, and pulses the chip
# reset GPIO31 every ~7 s while none does. The proxy runs with "-tag silabs_ncp_bt" so the daemon
# leaves it alone; when the proxy exits, the daemon brings Xiaomi's app back by itself.

DIR=/data
BIN=$DIR/gw3-btproxy
MODE_FILE=$DIR/gw3-btproxy.mode
HOOK=$DIR/scripts/startup.sh

hook_installed() { [ -x $HOOK ] && grep -q "gw3-btproxy.sh restore" $HOOK; }

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
restore)
	[ "$(cat $MODE_FILE 2>/dev/null)" = proxy ] && start
	;;
status)
	hook_installed || { [ "$(cat $MODE_FILE 2>/dev/null)" = proxy ] && start; }
	;;
*)
	echo "usage: $0 on|off|restore|status" >&2
	exit 1
	;;
esac
[ -n "$(proxy_pid)" ] && echo on || echo off
