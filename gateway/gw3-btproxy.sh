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
#
# While switching, the daemon is paused (SIGSTOP). Whatever ends this script, the daemon is resumed:
# hangups (telnet closed) are ignored, and the EXIT trap resumes it. One run at a time (lock). A run killed
# with -9 leaves its lock behind; the next run takes the lock over and resumes the daemon.

DIR=/data
BIN=$DIR/gw3-btproxy
MODE_FILE=$DIR/gw3-btproxy.mode
HOOK=$DIR/scripts/startup.sh
LOCK=/tmp/gw3-btproxy.lock # /tmp is in RAM: no lock survives a reboot

hook_installed() { [ -x $HOOK ] && grep -q "^[^#]*gw3-btproxy.sh restore" $HOOK; }

proxy_pid() { ps -ww | grep "$BIN -tag" | grep -v grep | awk '{print $1}'; }
daemon_pid() { ps -ww | grep daemon_miio.sh | grep -v grep | awk '{print $1}'; }
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
				# only one run removes a dead holder's lock, and only if it is still that one. It resumes the
				# daemon right here: another run may take the freed lock first, and it knows nothing of the pause.
				GC=1
				if [ "$(readlink $LOCK 2>/dev/null)" = "$H" ]; then
					for d in $(daemon_pid); do kill -CONT $d 2>/dev/null; done # before freeing the lock
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
	for d in $D; do kill -CONT $d 2>/dev/null; done
	[ -n "$GC" ] && rm -r $LOCK.gc 2>/dev/null
	[ -n "$LOCKED" ] && [ "$(readlink $LOCK 2>/dev/null)" = "$ME" ] && rm -f $LOCK
}
trap "" HUP
trap cleanup EXIT
trap "exit 1" INT TERM

xiaomi_pid() { ps -ww | grep silabs_ncp_bt | grep -v grep | grep -v "$BIN" | awk '{print $1}'; }

start_once() {
	# When neither app runs, the daemon is about to restart Xiaomi's. Pausing it halfway lets it finish
	# after the resume (reset pulse, then Xiaomi's app on the UART): wait until it has, then it is idle.
	i=0
	while [ -z "$(xiaomi_pid)" ] && [ $i -lt 15 ]; do sleep 1; i=$((i + 1)); done
	D=$(daemon_pid)
	[ -n "$D" ] && kill -STOP $D
	killall silabs_ncp_bt 2>/dev/null
	sleep 2
	killall -9 silabs_ncp_bt 2>/dev/null # also one the daemon started just before it was paused
	echo 1 > /sys/class/gpio/gpio31/value
	(trap "" HUP; $BIN -tag silabs_ncp_bt 2>&1 | logger -t "<BTP>") </dev/null >/dev/null 2>&1 &
	sleep 3
	for d in $D; do kill -CONT $d 2>/dev/null; done
	D=
	sleep 5
	[ -n "$(proxy_pid)" ]
}

# If the proxy still does not stay up (a daemon cycle that started anyway), the next try finds the daemon idle.
start() {
	[ -n "$(proxy_pid)" ] && return 0
	for try in 1 2 3; do
		start_once && return 0
		logger -t "<BTP>" "proxy did not stay up (try $try)"
	done
	return 1
}

stop() {
	P=$(proxy_pid)
	[ -n "$P" ] || return 0
	kill $P
	i=0
	while [ -n "$(proxy_pid)" ] && [ $i -lt 10 ]; do sleep 1; i=$((i + 1)); done
}

case "$1" in
on | off | restore | status) ;;
*)
	echo "usage: $0 on|off|restore|status" >&2
	exit 1
	;;
esac

# the boot restore holds the lock up to ~2 min (waits for the daemon, then up to 3 tries); status never waits.
# 120 s of waiting plus our own 3 tries stays under Home Assistant's 240 s.
WAIT=120
[ "$1" = status ] && WAIT=0
if lock $WAIT; then
	case "$1" in
	on)
		set_mode proxy || { echo "cannot save the mode (is /data full?)" >&2; exit 1; }
		# did not stay up: back to Xiaomi mode, so nothing keeps retrying while the switch shows off
		start || set_mode xiaomi
		;;
	off)
		set_mode xiaomi || { echo "cannot save the mode (is /data full?)" >&2; exit 1; }
		stop
		;;
	restore)
		# at boot: wait for Xiaomi's daemon, so it cannot reset the chip under a fresh proxy; then a few tries
		i=0
		while [ -z "$(daemon_pid)" ] && [ $i -lt 60 ]; do sleep 1; i=$((i + 1)); done
		[ "$(mode)" = proxy ] && start
		;;
	status)
		hook_installed || { [ "$(mode)" = proxy ] && start; }
		;;
	esac
elif [ "$1" != status ]; then
	echo "busy: another gw3-btproxy.sh run holds $LOCK"
	exit 1
fi
[ -n "$(proxy_pid)" ] && echo on || echo off
