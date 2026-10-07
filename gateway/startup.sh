#!/bin/sh
# Boot hook for gw3-btproxy, installed as /data/scripts/startup.sh.
# /etc/init.d/rcS runs this file INSTEAD of the stock startup.sh when it is executable, so the stock
# startup runs first, called the same way rcS calls it. Nothing below can delay or stop it.
startup.sh
# Once the stock apps are up, restore the saved Bluetooth mode (proxy or Xiaomi app), in the background.
(sleep 60; [ -x /data/gw3-btproxy.sh ] && sh /data/gw3-btproxy.sh restore) >/dev/null 2>&1 &
exit 0
