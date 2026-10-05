#!/bin/bash
# Record what Xiaomi's silabs_ncp_bt says to the BT chip: run it on the pty /dev/ttyp8,
# bridge /dev/ptyp8 (TCP 6002) and /dev/ttyS1 (TCP 6000), relay + log on this Mac. Always restores (trap).
set -u
cd "$(dirname "$0")"
step() { echo; echo "=== $(date +%H:%M:%S) $*"; }

restore() {
  step "restore Xiaomi BT"
  bun gwsh.ts 'killall silabs_ncp_bt; for p in 6000 6002; do ps w | grep "nc -l -p $p" | grep -v grep | awk "{print \$1}" | xargs kill 2>/dev/null; done; stty -F /dev/ttyS1 min 0 time 0; rm -f /tmp/bt_dont_need_startup; sleep 12; ps w | grep -e silabs -e "nc -l" | grep -v grep; free | head -2' 30
}

step "stop Xiaomi BT, start bridges"
trap restore EXIT
bun gwsh.ts 'touch /tmp/bt_dont_need_startup; killall silabs_ncp_bt; sleep 2; stty -F /dev/ttyS1 min 1 time 0; (trap "" HUP; exec nc -l -p 6000 </dev/ttyS1 >/dev/ttyS1 2>/dev/null) & (trap "" HUP; exec nc -l -p 6002 <>/dev/ptyp8 >&0 2>/dev/null) & sleep 1; ps w | grep "nc -l" | grep -v grep'

step "start relay, then Xiaomi BT app on the pty"
bun sniff.ts "${1:-90}" &
RELAY=$!
sleep 2
bun gwsh.ts '(trap "" HUP; exec 3<>/dev/ttyp8; stty -F /dev/ttyp8 raw -echo; silabs_ncp_bt /dev/ttyp8 1 2>&1 | logger -t "<BT>") & sleep 2; ps w | grep silabs | grep -v grep'
wait $RELAY
