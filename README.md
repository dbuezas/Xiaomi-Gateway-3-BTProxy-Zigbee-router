# Xiaomi Gateway 3: Bluetooth proxy & Zigbee router

[![Open your Home Assistant instance and open this repository in HACS.](https://my.home-assistant.io/badges/hacs_repository.svg)](https://my.home-assistant.io/redirect/hacs_repository/?owner=dbuezas&repository=Xiaomi-Gateway-3-BTProxy-Zigbee-router&category=integration)

A Home Assistant integration that puts both radios of a Xiaomi Gateway 3 (ZNDMWG03LM) to work in a
standard Home Assistant setup:

- **Bluetooth proxy:** the gateway becomes an ESPHome-compatible Bluetooth proxy. Home Assistant uses
  it like any ESPHome proxy, for every Bluetooth device, not only Xiaomi ones (I use it for my eQ-3
  thermostats). The proxy runs on the gateway itself.
- **Zigbee router:** the gateway's Zigbee chip joins your existing ZHA network as a router and extends
  the mesh. In ZHA it is a named device: the gateway's LED as a colour light, an Identify button, and the gateway's
  button as a Zigbee remote. This part runs on the gateway itself too.

Both are optional and independent.

## Requirements

- Xiaomi Gateway 3 (ZNDMWG03LM) set up with the
  [Xiaomi Gateway 3 integration](https://github.com/AlexxIT/XiaomiGateway3), which opens telnet on it.
  Only this model (`lumi.gateway.mgl03`): the integration refuses other gateways, because their
  Bluetooth chip, reset pin and boot scripts may differ and installing could harm them.
- One of the gateway firmwares in the table below. The integration refuses any other one, and the switch script
  on the gateway does not start the proxy on any other one either (for example after a firmware update).
- For the Zigbee router: ZHA with its own coordinator, and the Xiaomi Gateway 3 integration's Zigbee
  mode set to **ZHA**. Devices paired to the gateway's own Zigbee network then need re-pairing to ZHA.
- The gateway must reach Home Assistant on a TCP port: to install, Home Assistant serves the files from a
  short-lived HTTP server on a random port and the gateway downloads them. Fine for Home Assistant OS and host
  networking; with Docker bridge networking or a host firewall, the install keeps failing ("download ... failed").

## Install

1. Click the button above (or add `https://github.com/dbuezas/Xiaomi-Gateway-3-BTProxy-Zigbee-router` as a custom repository in
   HACS, category Integration) and download it.
2. Restart Home Assistant.
3. [![Add the integration.](https://my.home-assistant.io/badges/config_flow_start.svg)](https://my.home-assistant.io/redirect/config_flow_start/?domain=gw3_btproxy)
   Enter the gateway's IP address (it is suggested if the Xiaomi Gateway 3 integration is set up).

The integration copies the proxy and its two scripts to the gateway's `/data` and keeps them up to date.

## Use

The device "Xiaomi Gateway 3 radios" has three entities:

| Entity | What it does |
|---|---|
| **Bluetooth proxy** (switch) | On: the proxy owns the gateway's BT chip; the integration adds it to ESPHome. Off: Xiaomi's own BT app runs, as before. |
| **Zigbee router** (switch) | On: the gateway's Zigbee chip is a router in your ZHA network (it opens ZHA pairing for 2 minutes when it has to join). Off: it leaves ZHA and gets the gateway's own Zigbee network back. |
| **Zigbee router state** (sensor) | `off`, `up` or `error`. After a gateway reboot the integration brings the router back by itself. |

Both settings persist: the Bluetooth mode is saved on the gateway, the Zigbee router setting in the integration.

### The router in ZHA

ZHA shows the router as **Xiaomi Gateway 3 Zigbee router**, online, with these entities:

| Entity | What it does |
|---|---|
| **Light** | The gateway's RGB LED. Each of red, green and blue is only on or off, so it shows 7 colours: a colour picked in Home Assistant becomes the nearest of them. No brightness. On shows the last colour (white at first). Xiaomi's own app also sets it (blue while it runs). ZHA names it after the device, so I renamed mine to "LED". |
| **Identify** (button) | Blinks the LED white. |

The gateway's button (endpoint 2): a short press (under 2 s) sends an On/Off Toggle command. ZHA turns it into a
`zha_event` (command `toggle`, endpoint 2) for automations. The command also goes to every device bound to that
endpoint's On/Off cluster in ZHA, without WiFi or Home Assistant in the path. Long presses stay Xiaomi's. I tested
the button with ZHA; I did not test bindings yet.

When ZHA does not know the router's current endpoints (it met the router before 0.5.0, as "unk_manufacturer
unk_model", or an older layout), the integration asks ZHA once to read it again. ZHA's **Reconfigure** button does
the same.

The integration's device "Xiaomi Gateway 3 radios" has the gateway's MAC address and, once the router is up, the
chip's Zigbee address as connections. Home Assistant links it by them with the Xiaomi Gateway 3 integration's device
and with ZHA's.

### Going back to the gateway's own Zigbee network

Before the chip first joins ZHA, switching the Zigbee router on saves a backup of the gateway's own Zigbee network
(zigpy open coordinator format, with the network key and the device list) to `/config/.storage/gw3_btproxy/`.
The backup is read back and checked (same chip, same network, a network key) before anything else happens; if it
fails, the chip does not join. Switching it off leaves ZHA and writes the newest backup of that chip back into it.
Then set the Xiaomi Gateway 3 integration's Zigbee mode back to Mi Home. If your config folder is in git, ignore that
folder: the backups hold the network key (the files are readable by their owner only).

Safety checks: the switch refuses when ZHA uses this chip as its own radio, and does nothing when it is already
off. If the chip joined ZHA with version 0.2.x, there is no backup: switching off then leaves it with no network
(a notification says so), and its devices must be paired again in Mi Home.

Before removing the integration, switch the Zigbee router off: otherwise the chip stays a ZHA router that nothing
brings back after the next gateway reboot.

### Firmwares

Each one tested on my gateway: switching both ways, a proxy crash (Xiaomi's app comes back), a reboot with the
boot hook, the API test (adverts, connect, GATT read, write, notify), cancelling a pending connect, the Zigbee
router. The Zigbee router's host on the gateway is tested on 1.5.4_0090 only.

| Firmware | Bluetooth chip | Hardware flow control | Lost bytes in 3 min |
| --- | --- | --- | --- |
| 1.5.4_0090, 1.5.7_0001 | 1.4.0 | yes | 0 |

Updating the gateway firmware also flashes the Bluetooth chip. Not supported: everything before 1.5.4 (another
watchdog and chip reset, and no hardware flow control: the gateway loses Bluetooth bytes; version 0.4.0 of this
integration still supports 1.5.0_0026, 1.5.0_0102 and 1.5.1_0032), and 1.5.5, 1.5.6 (I could not get them to
test). AlexxIT's integration lists 1.5.7 as untested; on my gateway it worked, including Zigbee in ZHA mode.

On a firmware that is not in the list (also after a firmware update), the integration does not load, so its
switches are gone, and the script does not start the proxy (Xiaomi's app keeps the Bluetooth chip). So before
updating the gateway firmware, switch the Zigbee router off: only the switch gives the chip its own network back.
If it is too late for that, by hand, over telnet:

- `sh /data/gw3-btproxy.sh off`: Xiaomi's Bluetooth app (already the case on such a firmware).
- In `/data/scripts/startup.sh`, remove the `gw3-btproxy.sh restore` line (or the file, if it holds only this hook;
  it also keeps telnet open).
- Zigbee: the chip stays a router in your ZHA network. Remove the gateway from ZHA's devices. Its own network is
  not restored by that: it is in the backup in `/config/.storage/gw3_btproxy/` (restore it with zigpy-cli, or
  install a tested firmware and use the switch); then set the Xiaomi Gateway 3 integration's Zigbee mode back to
  Mi Home. Without that, the gateway's own Zigbee devices need pairing again.

### Xiaomi Bluetooth sensors

With the proxy on, Xiaomi's app no longer receives Bluetooth, so the Xiaomi Gateway 3 integration's
BLE sensors stop updating. Add them to Home Assistant's own **Xiaomi BLE** integration instead (it
needs each sensor's bindkey). Switch the proxy off whenever you want to pair a new Xiaomi device in
the Mi Home app.

## How it works

### Bluetooth

`gw3-btproxy` is a single static Go binary (MIPS, about 6 MB RAM) that owns the BT chip on `/dev/ttyS1`
and speaks the ESPHome native API (plaintext) on port 6053. It supports raw advertisements, passive
and active scanning, active connections (at most 2 at a time, a chip limit), GATT discovery, read,
write, descriptors, notifications and indications. It does not support cache clearing or encryption,
and it cannot pair on this chip firmware.

Its source, build, options and what I found about the chip are in [`btproxy/`](btproxy/README.md).

`gateway/gw3-btproxy.sh on|off|restore|restart|status` switches between the two apps on the gateway; the mode is
saved in `/data/gw3-btproxy.mode`.

### Optional: start the proxy on the gateway's own boot

Without this, the integration restores proxy mode within ~2 minutes after a gateway reboot (it needs
Home Assistant and telnet). With it, the gateway restores it by itself.

`/etc/init.d/rcS` runs `/data/scripts/startup.sh` **instead of** the stock `startup.sh` when that file is
executable. `gateway/startup.sh` therefore runs the stock `startup.sh` first, then runs
`gw3-btproxy.sh restore` in the background after 60 s. With the hook installed, the integration's poll
only reads the state. The hook also starts `telnetd` (after turning the tty back on, which the stock startup
turns off, as the Xiaomi Gateway 3 integration's own telnet command does), so telnet stays reachable whatever
the firmware's own way of opening it does. Tested on every firmware in the table above. A broken hook could
keep the gateway from booting
normally, so install it carefully:

0. Check first: `grep -n scripts /etc/init.d/rcS` shows the `CUSTOM_STARTUP=/data/scripts/startup.sh` line, and
   `/data/scripts/startup.sh` does not exist yet (if it does, merge the `restore` line into it instead).
1. Write it as `/data/scripts/startup.sh.new` (not executable yet; `mkdir -p /data/scripts` first). The gateway
   has no scp: serve the file from your computer (`python3 -m http.server` in `gateway/`) and run
   `wget -O /data/scripts/startup.sh.new http://<your computer>:8000/startup.sh` on the gateway.
2. Check it on the gateway: `sh -n`, the checksum matches this file, the first line is `#!/bin/sh`, no
   CR line endings.
3. Only then: `chmod 755 /data/scripts/startup.sh.new && mv /data/scripts/startup.sh.new /data/scripts/startup.sh`.

To undo, delete `/data/scripts/startup.sh`.

### Zigbee

In ZHA mode, openmiio_agent serves the gateway's Zigbee chip (EmberZNet 6.6.2 NCP, EZSP v7) on TCP port
8888, one connection at a time. Every connection resets the chip. The chip keeps its network across resets, but
like every NCP it only brings it up when a host calls `networkInit`, with stack profile 2 and security level 5 set
first (otherwise it answers NOT_JOINED). Two programs talk to it:

- **The router host**, `gw3-btproxy zigbee` on the gateway, most of the time. It brings the network up, adds
  two endpoints and answers them: 1 the LED (Basic, Identify, On/Off, Level Control, Color Control), 2 the button
  (an On/Off client, on its own endpoint marked as a switch: on the light's endpoint ZHA adds an "Opening" sensor
  for it). It first checks that the chip
  is a router in ZHA's network (the integration passes the extended PAN ID, PAN ID and channel); otherwise it resets
  the chip and gives up. When its connection drops (openmiio_agent restarted), it connects again by itself. It uses
  about 3.3 MB of RAM. `gateway/gw3-zigbee.sh start|stop|status` runs it; its log goes to syslog (`<ZB>`).
- **The integration**, with `bellows` (the library ZHA uses for these chips), to join, leave, back up and restore.
  It stops the host first. Joining uses the well-known default trust center link key (`ZigBeeAlliance09`), as ZHA
  expects.

While nobody is connected, the chip keeps routing. After a gateway reboot the integration starts the host again
within ~2 minutes (it needs Home Assistant and telnet for that).

After a reset the chip has few packet buffers: the messages it sends are garbled after their first 11 bytes until a
host sets `CONFIG_PACKET_BUFFER_COUNT` to 0xFF (all free memory), as bellows does. The host and the integration both
set it. Up to 0.4.x the router ran with the default buffers (I did not check whether that also garbled the messages
it relayed).

## Development

```sh
btproxy/build.sh   # builds the proxy and bundles it into custom_components/gw3_btproxy/bin
```

The build is reproducible, and [`btproxy/README.md`](btproxy/README.md) explains how to check the bundled
binary against the source and how to run the proxy on another machine against the gateway's chip.

`dev/` has the tools I used (all take the gateway address from `GW`):

- `gwsh.ts`: run a command on the gateway over telnet.
- `devtest.sh`: stop Xiaomi's app, bridge the UART, run the proxy here, test it with `apitest.py`, restore.
- `sniff.sh`: run Xiaomi's app through this machine and log every BGAPI frame.
- `ha_ws.py`: a stdlib Home Assistant websocket client.

`apitest.py` needs `pip install aioesphomeapi`.
