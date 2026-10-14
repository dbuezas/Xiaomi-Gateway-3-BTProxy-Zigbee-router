#!/bin/sh
# Start, stop and query the Zigbee router host ("gw3-btproxy zigbee") on the gateway.
#   gw3-zigbee.sh start <extended PAN id> <PAN id> <channel>
#                         start it unless it runs, then wait (up to 30 s) until it is up or gives up
#   gw3-zigbee.sh stop    stop it; it has let go of the chip (TCP 8888) when this returns
#   gw3-zigbee.sh status  print its status
# The last line is always the status: "<state> pid=... version=... [ieee=... nwk=...]" while it runs ("up",
# "starting", "error <why>"), "exited <its last status>" after it ended, "none" when it has not run since boot.

BIN=/data/gw3-btproxy
STATUS=/tmp/gw3-zigbee.status # /tmp is in RAM: gone after a reboot

host_pid() { ps -ww | grep "$BIN zigbee" | grep -v grep | awk '{print $1}'; }

status() {
	if [ -n "$(host_pid)" ]; then
		cat $STATUS 2>/dev/null || echo starting
	elif [ -f $STATUS ]; then
		echo "exited $(cat $STATUS)"
	else
		echo none
	fi
}

start() {
	[ -n "$(host_pid)" ] && return 0
	rm -f $STATUS
	# its log goes to syslog ("<ZB>"); hangups (telnet closed) are ignored
	(trap "" HUP; $BIN zigbee -status $STATUS -epid "$1" -pan "$2" -channel "$3" 2>&1 | logger -t "<ZB>") \
		</dev/null >/dev/null 2>&1 &
	i=0
	while [ $i -lt 30 ]; do
		sleep 1
		i=$((i + 1))
		[ -n "$(host_pid)" ] || return 1 # ended: refused (see the status)
		case "$(cat $STATUS 2>/dev/null)" in up*) return 0 ;; esac
	done
	return 1 # still starting or retrying: it keeps trying by itself
}

stop() {
	P=$(host_pid)
	[ -n "$P" ] || return 0
	kill $P
	i=0
	while [ -n "$(host_pid)" ] && [ $i -lt 10 ]; do sleep 1; i=$((i + 1)); done
	P=$(host_pid)
	[ -n "$P" ] && kill -9 $P && sleep 1
	[ -z "$(host_pid)" ]
}

case "$1" in
start)
	[ $# -eq 4 ] || { echo "usage: $0 start <extended PAN id> <PAN id> <channel>" >&2; exit 1; }
	start "$2" "$3" "$4"
	;;
stop) stop || echo "could not stop pid $(host_pid)" ;;
status) ;;
*)
	echo "usage: $0 start <extended PAN id> <PAN id> <channel>|stop|status" >&2
	exit 1
	;;
esac
status
