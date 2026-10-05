#!/bin/bash
# Run gw3-btproxy on this Mac against the gateway chip (via an nc bridge) and test it with apitest.py.
set -u
cd "$(dirname "$0")"
step() { echo; echo "=== $(date +%H:%M:%S) $*"; }
restore() {
  [ -n "${PROXY:-}" ] && kill $PROXY 2>/dev/null
  step "restore Xiaomi BT"
  bun gwsh.ts 'ps w | grep "nc -l -p 6000" | grep -v grep | awk "{print \$1}" | xargs kill 2>/dev/null; stty -F /dev/ttyS1 min 0 time 0; kill -CONT $(ps w | grep daemon_miio.sh | grep -v grep | awk "{print \$1}"); sleep 12; ps w | grep silabs | grep -v grep || echo "!! silabs_ncp_bt NOT running"' 30 | tail -1
}
step "stop Xiaomi BT, start bridge"
trap restore EXIT
bun gwsh.ts 'kill -STOP $(ps w | grep daemon_miio.sh | grep -v grep | awk "{print \$1}"); killall silabs_ncp_bt; sleep 2; echo 1 > /sys/class/gpio/gpio31/value; stty -F /dev/ttyS1 min 1 time 0; (trap "" HUP; exec nc -l -p 6000 </dev/ttyS1 >/dev/ttyS1 2>/dev/null) & sleep 1; ps w | grep "nc -l" | grep -v grep' | tail -1
step "start proxy"
../gw3-btproxy -tcp ${GW:?set GW to the gateway IP}:6000 -listen 127.0.0.1:6053 ${PROXYARGS:-} > proxy.log 2>&1 &
PROXY=$!
for i in $(seq 1 20); do grep -q ready proxy.log && break; sleep 0.5; done
cat proxy.log
step "API test"
${PYTHON:-venv/bin/python} apitest.py 127.0.0.1 6053 "$@"
step "proxy log"
tail -30 proxy.log
