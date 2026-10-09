#!/bin/sh
# Boot hook for gw3-btproxy, installed as /data/scripts/startup.sh.
# /etc/init.d/rcS runs this file INSTEAD of the stock startup.sh when it is executable, so the stock
# startup runs first, called the same way rcS calls it. Nothing below can delay or stop it.
# (By path when it is there: a PATH that finds this file first would make it call itself.)
if [ -x /bin/startup.sh ]; then /bin/startup.sh; else startup.sh; fi
# Telnet on every boot, whatever the firmware's own way of opening it does (a safety net for updates). The stock
# startup ends with "echo disable > /sys/class/tty/tty/enable", which also stops telnet logins: turn it back on
# first, as the Xiaomi Gateway 3 integration's own telnet command does. In the background, after the stock
# startup; when telnetd already runs, the second one exits at once.
(echo enable > /sys/class/tty/tty/enable; telnetd) >/dev/null 2>&1 &
# Once the stock apps are up, restore the saved Bluetooth mode (proxy or Xiaomi app), in the background.
(sleep 60; [ -x /data/gw3-btproxy.sh ] && sh /data/gw3-btproxy.sh restore) >/dev/null 2>&1 &
exit 0
