#!/bin/sh
# Switch the gateway's BT chip between Xiaomi's silabs_ncp_bt and gw3-btproxy.
#   gw3-btproxy.sh on       proxy mode (saved)
#   gw3-btproxy.sh off      Xiaomi mode (saved)
#   gw3-btproxy.sh restore  in proxy mode, start the proxy if it is not running (the boot hook calls this)
#   gw3-btproxy.sh restart  in proxy mode, stop the running proxy and start it again (after an update)
#   gw3-btproxy.sh status   prints on/off. Read-only when the boot hook is installed; without it, it also
#                           restores (so the Home Assistant poll brings the proxy back after a reboot)
#
# Xiaomi's watchdog app_monitor.sh restarts Xiaomi's app (silabs_ncp_bt) every 5 s when it is not running, but not
# while /tmp/bt_dont_need_startup exists. So the flag is set before the switch and removed when the proxy exits (the
# watchdog then starts Xiaomi's app, with its own chip reset). The chip is reset the way the firmware does it
# (reset_bt_target.sh 1: GPIO37 high = start the application, not the bootloader; then GPIO31 pulsed).
# Hangups (telnet closed) are ignored. A run killed with -9 leaves its lock behind; the next run takes it over.
#
# One run at a time (lock).

DIR=/data
BIN=$DIR/gw3-btproxy
MODE_FILE=$DIR/gw3-btproxy.mode
HOOK=$DIR/scripts/startup.sh
LOCK=/tmp/gw3-btproxy.lock # /tmp is in RAM: no lock survives a reboot
NOSTART=/tmp/bt_dont_need_startup # app_monitor.sh leaves Xiaomi's app stopped while this exists
# Firmwares tested on the device; the proxy is not started on any other (Xiaomi's app keeps the chip). The same
# list as SUPPORTED_FIRMWARES in the integration's const.py (build.sh checks that they match).
SUPPORTED_FW="1.5.4_0090 1.5.7_0001"
FW=$(grep ^version= /etc/rootfs_fw_info 2>/dev/null | cut -d= -f2)
fw_ok() { case " $SUPPORTED_FW " in *" $FW "*) return 0 ;; esac; return 1; }

hook_installed() { [ -x $HOOK ] && grep -q "^[^#]*gw3-btproxy.sh restore" $HOOK; }

# "$BIN -": the proxy, whatever its flags (not the Zigbee host, "$BIN zigbee")
proxy_pid() { ps -ww | grep "$BIN -" | grep -v grep | awk '{print $1}'; }
watchdog_pid() { ps -ww | grep app_monitor.sh | grep -v grep | awk '{print $1}'; }
mode() { cat $MODE_FILE 2>/dev/null; }
set_mode() { [ "$(mode)" = "$1" ] || { echo "$1" > $MODE_FILE.new && mv $MODE_FILE.new $MODE_FILE; }; }

# The lock is a symlink to "pid:start time" of its holder: created atomically with its content, and a
# reused pid does not look alive.
me() { echo "$$:$(awk '{print $22}' /proc/$$/stat)"; }
alive() { [ -n "${1%%:*}" ] && [ "$(awk '{print $22}' /proc/${1%%:*}/stat 2>/dev/null)" = "${1#*:}" ]; }

lock() { # $1: seconds to wait
	ME=$(me)
	i=0
	gcwait=0
	while ! ln -s "$ME" $LOCK 2>/dev/null; do
		H=$(readlink $LOCK 2>/dev/null)
		if [ -n "$H" ] && ! alive "$H"; then
			if mkdir $LOCK.gc 2>/dev/null; then
				# only one run removes a dead holder's lock, and only if it is still that one. Without a proxy, it
				# lets the watchdog bring Xiaomi's app back (the dead run may have stopped it).
				GC=1
				if [ "$(readlink $LOCK 2>/dev/null)" = "$H" ]; then
					[ -z "$(proxy_pid)" ] && rm -f $NOSTART # before freeing the lock
					rm -f $LOCK
				fi
				GC=
				rm -r $LOCK.gc
				gcwait=0
				continue
			fi
			# the takeover guard is held for a moment only: seen on 5 passes in a row (~4 s), its run died
			gcwait=$((gcwait + 1))
			if [ $gcwait -ge 5 ]; then
				rm -r $LOCK.gc 2>/dev/null
				gcwait=0
				continue
			fi
		else
			gcwait=0
		fi
		i=$((i + 1))
		[ $i -ge "$1" ] && return 1
		sleep 1
	done
	LOCKED=1
}

cleanup() {
	# killed between taking Xiaomi's app down and starting the proxy: let the watchdog bring Xiaomi's app back
	# (only a run that held the lock: another one may be in the middle of a switch)
	[ -n "$LOCKED" ] && [ -z "$(proxy_pid)" ] && rm -f $NOSTART
	[ -n "$GC" ] && rm -r $LOCK.gc 2>/dev/null
	[ -n "$LOCKED" ] && [ "$(readlink $LOCK 2>/dev/null)" = "$ME" ] && rm -f $LOCK
}
trap "" HUP
trap cleanup EXIT
trap "exit 1" INT TERM

# up: the proxy has set the chip up and listens for Home Assistant (port 6053). Init can take ~12 s (hello retries,
# a reset, the boot event); a proxy that cannot talk to the chip exits during it.
wait_up() {
	i=0
	while [ $i -lt 15 ]; do
		[ -n "$(proxy_pid)" ] || return 1
		netstat -tln 2>/dev/null | grep -q ":6053 " && return 0
		sleep 1
		i=$((i + 1))
	done
	return 1
}

launch() {
	# When the proxy exits (stopped or crashed), the watchdog may start Xiaomi's app again: remove the flag, but only
	# if it is still this launch's (a newer start may have set its own meanwhile).
	T=$TOKEN
	(trap "" HUP; $BIN -serial /dev/ttyS1 2>&1 | logger -t "<BTP>"
		[ "$(cat $NOSTART 2>/dev/null)" = "$T" ] && rm -f $NOSTART) \
		</dev/null >/dev/null 2>&1 &
}

start_once() {
	TOKEN=$(cut -d' ' -f1 /proc/uptime)$$
	echo "$TOKEN" > $NOSTART
	killall silabs_ncp_bt 2>/dev/null
	sleep 2
	killall -9 silabs_ncp_bt 2>/dev/null
	reset_bt_target.sh 1 >/dev/null 2>&1 # GPIO37 high, GPIO31 pulsed: the chip starts its application
	launch
	wait_up
}

start() {
	[ -n "$(proxy_pid)" ] && return 0
	if ! fw_ok; then
		logger -t "<BTP>" "firmware ${FW:-unknown} is not tested: the proxy is not started"
		return 1
	fi
	for try in 1 2 3; do
		start_once && return 0
		logger -t "<BTP>" "proxy did not stay up (try $try)"
		kill_proxy || return 1 # one that hangs while setting the chip up: never two on the UART
	done
	return 1
}

kill_proxy() {
	P=$(proxy_pid)
	[ -n "$P" ] || return 0
	kill $P
	i=0
	while [ -n "$(proxy_pid)" ] && [ $i -lt 10 ]; do sleep 1; i=$((i + 1)); done
	P=$(proxy_pid)
	[ -n "$P" ] || return 0
	kill -9 $P
	sleep 1
	[ -z "$(proxy_pid)" ] # one that survives -9 (stuck in the kernel): do not start another one
}

stop() {
	kill_proxy
	rm -f $NOSTART # the watchdog starts Xiaomi's app within 5 s
}

case "$1" in
on | off | restore | restart | status) ;;
*)
	echo "usage: $0 on|off|restore|restart|status" >&2
	exit 1
	;;
esac

# A run holds the lock up to ~3 min (the boot restore: up to 60 s for the watchdog, then 3 tries); status never waits.
# 90 s of waiting plus this run's own 3 tries (~3 x 20 s) stays under Home Assistant's 270 s.
WAIT=90
[ "$1" = status ] && WAIT=0
if lock $WAIT; then
	case "$1" in
	on)
		fw_ok || { echo "unsupported firmware ${FW:-unknown} (tested: $SUPPORTED_FW)"; exit 1; }
		set_mode proxy || { echo "cannot save the mode (is /data full?)" >&2; exit 1; }
		# did not stay up: back to Xiaomi mode, so nothing keeps retrying while the switch shows off
		start || { stop; set_mode xiaomi; }
		;;
	off)
		set_mode xiaomi || { echo "cannot save the mode (is /data full?)" >&2; exit 1; }
		stop
		;;
	restore)
		# at boot: wait for Xiaomi's watchdog, so it cannot reset the chip under a fresh proxy; then a few tries
		i=0
		while [ -z "$(watchdog_pid)" ] && [ $i -lt 60 ]; do sleep 1; i=$((i + 1)); done
		[ "$(mode)" = proxy ] && fw_ok && start
		;;
	restart)
		# after an update: the new binary is in place, the old one still runs
		[ "$(mode)" = proxy ] && { kill_proxy && start || { stop; set_mode xiaomi; }; }
		;;
	status)
		hook_installed || { [ "$(mode)" = proxy ] && fw_ok && start; }
		;;
	esac
elif [ "$1" != status ]; then
	echo "busy: another gw3-btproxy.sh run holds $LOCK"
	exit 1
fi
[ -n "$(proxy_pid)" ] && echo on || echo off
