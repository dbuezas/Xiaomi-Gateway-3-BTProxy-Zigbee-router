# gw3-btproxy

The Bluetooth proxy that runs on the Xiaomi Gateway 3: a single static Go binary (MIPS, about 6 MB RAM) that
owns the gateway's Bluetooth chip on `/dev/ttyS1` and speaks the ESPHome native API (plaintext) on port 6053.
Home Assistant sees it as an ESPHome Bluetooth proxy.

It supports raw advertisements, passive and active scanning, active connections (at most 2 at a time, a chip
limit), GATT discovery, read, write, descriptors, notifications and indications. It does not support cache
clearing or encryption, and it cannot pair (see below).

The Home Assistant integration in [`custom_components/gw3_btproxy`](../custom_components/gw3_btproxy) installs
this binary on the gateway and switches between it and Xiaomi's own Bluetooth app; see the
[main README](../README.md).

## Source

| File | What it does |
| --- | --- |
| `main.go` | Flags, startup, the serial port or TCP bridge |
| `bgapi.go` | BGAPI over the UART: SLIP from the chip, raw commands to it, one command at a time |
| `proxy.go` | Scanning, connections (connect, retries, cancel, watchdog), GATT procedures |
| `pairing.go` | Pairing, used only when the chip firmware has a security manager |
| `api.go` | ESPHome native API server: clients, send queues, request dispatch |
| `proto.go` | The minimal protobuf encoding the API needs |
| `serial_linux.go`, `serial_other.go` | UART setup on the gateway; a stub elsewhere |

## Build

```sh
./build.sh
```

It builds `gw3-btproxy-mipsle` for the gateway (MIPS32 little endian, soft float) and `gw3-btproxy` for this
machine, and copies the gateway binary and `../gateway/gw3-btproxy.sh` into `../custom_components/gw3_btproxy/bin/`.

The build is reproducible: with the Go version in `go.mod` (1.26.2), it produces exactly the binary bundled in
the integration, so you can check that binary against this source:

```sh
./build.sh && md5sum gw3-btproxy-mipsle ../custom_components/gw3_btproxy/bin/gw3-btproxy
```

The version reported to Home Assistant comes from the integration's `manifest.json`.

## Options

| Flag | Default | |
| --- | --- | --- |
| `-serial` | `/dev/ttyS1` | the chip's UART |
| `-tcp host:port` | | use a TCP bridge to the UART instead (development) |
| `-listen` | `:6053` | ESPHome API address |
| `-name`, `-friendly-name` | `gw3-btproxy`, `Gateway BT Proxy` | names shown in Home Assistant |
| `-mac` | from the chip address | MAC reported to Home Assistant |
| `-max-conn` | `2` | simultaneous connections (the chip allows 2) |
| `-active` | off | active scanning (Home Assistant can also switch it) |
| `-mtu` | `247` | largest ATT MTU to offer (23 = never exchange) |
| `-passkeys` | `/data/gw3-btproxy.passkeys` | `MAC PIN` lines, for pairing |
| `-tag` | | ignored; `-tag silabs_ncp_bt` makes `daemon_miio.sh` treat the proxy as its BT app |
| `-v` | off | debug logging |

## What I found about the chip

- Silicon Labs chip, Bluetooth SDK 2.13.8 BGAPI at 115200 baud. Commands to the chip are raw BGAPI;
  everything from the chip is SLIP-framed. SLIP-framed commands fail with 0x0195 "command incomplete".
- Xiaomi's firmware only forwards Xiaomi adverts in the legacy scan reports. With
  `le_gap_set_discovery_extended_scan_response(1)` every advert comes through. The proxy sets it again
  every time it starts scanning.
- The firmware has no security manager: every pairing command answers 0x0183 "not implemented", so
  the chip cannot pair, and a device that insists on pairing (an eQ-3 thermostat with its PIN on, for
  example) hangs up on it. The proxy checks at start and offers pairing to Home Assistant only when the
  chip can do it; it then types in the PIN listed for the device in the passkeys file.
- `system_reset(1)` puts the chip in DFU mode and it goes silent until a GPIO reset. Use `system_reset(0)`.
- A link that drops right after opening (0x23e) works on a retry, so the proxy retries up to 3 times.
- The chip reports errors as 0x02xx (HCI) and 0x04xx (ATT); the proxy passes the bare codes on, as ESPHome does.
- `/bin/daemon_miio.sh` restarts Xiaomi's `silabs_ncp_bt` and pulses the chip reset (GPIO31) every ~7 s
  while no process with that name runs. The proxy therefore runs as `gw3-btproxy -tag silabs_ncp_bt`.
  If the proxy dies, the daemon brings Xiaomi's app back by itself.

## Run it on another machine

Against the chip through a TCP bridge on the gateway: `stty -F /dev/ttyS1 min 1 time 0`, then
`nc -l -p 6000 </dev/ttyS1 >/dev/ttyS1`, with Xiaomi's app stopped and `daemon_miio.sh` paused. Switch the
Bluetooth proxy off in Home Assistant first: without the boot hook, its poll would start the proxy on the UART in
the middle of the session.

```sh
./gw3-btproxy -tcp <gateway>:6000 -listen 127.0.0.1:6053 -v
```

[`../dev/devtest.sh`](../dev/devtest.sh) does all of this and runs the end-to-end test.
