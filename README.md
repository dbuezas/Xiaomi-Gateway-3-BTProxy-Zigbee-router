# Xiaomi Gateway 3: Bluetooth proxy & Zigbee router

[![Open your Home Assistant instance and open this repository in HACS.](https://my.home-assistant.io/badges/hacs_repository.svg)](https://my.home-assistant.io/redirect/hacs_repository/?owner=dbuezas&repository=Xiaomi-Gateway-3-BTProxy-Zigbee-router&category=integration)

A Home Assistant integration that puts both radios of a Xiaomi Gateway 3 (ZNDMWG03LM) to work in a
standard Home Assistant setup:

- **Bluetooth proxy:** the gateway becomes an ESPHome-compatible Bluetooth proxy. Home Assistant uses
  it like any ESPHome proxy, for every Bluetooth device, not only Xiaomi ones (I use it for my eQ-3
  thermostats). The proxy runs on the gateway itself.
- **Zigbee router:** the gateway's Zigbee chip joins your existing ZHA network as a router and extends
  the mesh.

Both are optional and independent.

## Requirements

- Xiaomi Gateway 3 (ZNDMWG03LM, tested with firmware 1.5.0_0102) set up with the
  [Xiaomi Gateway 3 integration](https://github.com/AlexxIT/XiaomiGateway3), which opens telnet on it.
- For the Zigbee router: ZHA with its own coordinator, and the Xiaomi Gateway 3 integration's Zigbee
  mode set to **ZHA**. Devices paired to the gateway's own Zigbee network then need re-pairing to ZHA.

## Install

1. Click the button above (or add `https://github.com/dbuezas/Xiaomi-Gateway-3-BTProxy-Zigbee-router` as a custom repository in
   HACS, category Integration) and download it.
2. Restart Home Assistant.
3. [![Add the integration.](https://my.home-assistant.io/badges/config_flow_start.svg)](https://my.home-assistant.io/redirect/config_flow_start/?domain=gw3_btproxy)
   Enter the gateway's IP address (it is suggested if the Xiaomi Gateway 3 integration is set up).

The integration copies the proxy to the gateway's `/data` and keeps it up to date.

## Use

The device "Xiaomi Gateway 3 radios" has three entities:

| Entity | What it does |
|---|---|
| **Bluetooth proxy** (switch) | On: the proxy owns the gateway's BT chip; the integration adds it to ESPHome. Off: Xiaomi's own BT app runs, as before. |
| **Join Zigbee network as router** (button) | Opens ZHA pairing for 2 minutes and joins the gateway's Zigbee chip as a router. |
| **Zigbee router** (sensor) | `off`, `up` or `error`. After a gateway reboot the integration brings the router back by itself. |

The Bluetooth mode is saved on the gateway and comes back after a reboot.

### Xiaomi Bluetooth sensors

With the proxy on, Xiaomi's app no longer receives Bluetooth, so the Xiaomi Gateway 3 integration's
BLE sensors stop updating. Add them to Home Assistant's own **Xiaomi BLE** integration instead (it
needs each sensor's bindkey). Switch the proxy off whenever you want to pair a new Xiaomi device in
the Mi Home app.

## How it works

### Bluetooth

`gw3-btproxy` is a single static Go binary (MIPS, about 5 MB RAM) that owns the BT chip on `/dev/ttyS1`
and speaks the ESPHome native API (plaintext) on port 6053. It supports raw advertisements, passive
and active scanning, active connections (at most 2 at a time, a chip limit), GATT discovery, read,
write, descriptors, notifications and indications. It does not support cache clearing or encryption,
and it cannot pair: see below.

What I found about the chip:

- Silicon Labs chip, Bluetooth SDK 2.13.8 BGAPI at 115200 baud. Commands to the chip are raw BGAPI;
  everything from the chip is SLIP-framed. SLIP-framed commands fail with 0x0195 "command incomplete".
- Xiaomi's firmware only forwards Xiaomi adverts in the legacy scan reports. With
  `le_gap_set_discovery_extended_scan_response(1)` every advert comes through. The proxy sets it again
  every time it starts scanning.
- The firmware has no security manager: every pairing command answers 0x0183 "not implemented", so
  the chip cannot pair, and a device that insists on pairing (an eQ-3 thermostat with its PIN on, for
  example) hangs up on it. The proxy checks at start and offers pairing to Home Assistant only when the
  chip can do it; it then types in the PIN listed for the device in `/data/gw3-btproxy.passkeys`
  (lines of `MAC PIN`).
- `system_reset(1)` puts the chip in DFU mode and it goes silent until a GPIO reset. Use `system_reset(0)`.
- A link that drops right after opening (0x23e) works on a retry, so the proxy retries up to 3 times.
- `/bin/daemon_miio.sh` restarts Xiaomi's `silabs_ncp_bt` and pulses the chip reset (GPIO31) every ~7 s
  while no process with that name runs. The proxy therefore runs as `gw3-btproxy -tag silabs_ncp_bt`.
  If the proxy dies, the daemon brings Xiaomi's app back by itself.

`gateway/gw3-btproxy.sh on|off|status` switches between the two apps on the gateway; the mode is
saved in `/data/gw3-btproxy.mode`.

### Optional: start the proxy on the gateway's own boot

Without this, the integration restores proxy mode within ~2 minutes after a gateway reboot (it needs
Home Assistant and telnet). With it, the gateway restores it by itself.

`/etc/init.d/rcS` runs `/data/scripts/startup.sh` **instead of** the stock `startup.sh` when that file is
executable. `gateway/startup.sh` therefore runs the stock `startup.sh` first, then runs
`gw3-btproxy.sh restore` in the background after 60 s. With the hook installed, the integration's poll
only reads the state; without it, the poll restores proxy mode within ~2 minutes after a reboot. A broken hook could keep the gateway from booting
normally, so install it carefully:

1. Write it as `/data/scripts/startup.sh.new` (not executable yet).
2. Check it on the gateway: `sh -n`, the checksum matches this file, the first line is `#!/bin/sh`, no
   CR line endings.
3. Only then: `chmod 755 /data/scripts/startup.sh.new && mv /data/scripts/startup.sh.new /data/scripts/startup.sh`.

To undo, delete `/data/scripts/startup.sh`.

### Zigbee

In ZHA mode, openmiio_agent serves the gateway's Zigbee chip (EmberZNet NCP, EZSP v7) on TCP port
8888. The integration talks to it with `bellows`, the library ZHA uses for these chips:

- Joining uses the well-known default trust center link key (`ZigBeeAlliance09`), as ZHA expects.
- Once joined, the chip routes on its own. No host needs to stay connected.
- The chip keeps the network across resets, but like every NCP it only brings it up when a host calls
  `networkInit`, with stack profile 2 and security level 5 set first (otherwise it answers NOT_JOINED).
  The integration does that whenever the gateway's boot time or the openmiio_agent process changes.
- Any connection to port 8888 resets the chip, so the integration only connects when needed.

## Development

```sh
./build.sh   # builds the proxy and bundles it into custom_components/gw3_btproxy/bin
```

Run the proxy on another machine against the chip through a TCP bridge on the gateway
(`nc -l -p 6000 </dev/ttyS1 >/dev/ttyS1`, with Xiaomi's app stopped and `daemon_miio.sh` paused):

```sh
./gw3-btproxy -tcp <gateway>:6000 -listen 127.0.0.1:6053 -v
```

`dev/` has the tools I used (all take the gateway address from `GW`):

- `gwsh.ts`: run a command on the gateway over telnet.
- `devtest.sh`: stop Xiaomi's app, bridge the UART, run the proxy here, test it with `apitest.py`, restore.
- `sniff.sh`: run Xiaomi's app through this machine and log every BGAPI frame.
- `ha_ws.py`: a stdlib Home Assistant websocket client.

`apitest.py` needs `pip install aioesphomeapi`.
