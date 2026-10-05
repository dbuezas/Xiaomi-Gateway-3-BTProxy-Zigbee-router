# End-to-end test of gw3-btproxy through the same library Home Assistant uses.
import asyncio, sys, time
from aioesphomeapi import APIClient

HOST, PORT = sys.argv[1], int(sys.argv[2])
TARGET = sys.argv[3] if len(sys.argv) > 3 else None
EQ3_WRITE = "3fa4585a-ce4a-3bad-db4b-b8df8179ea09"
EQ3_NOTIFY = "d0e8434d-cd29-0996-af41-6c90f4e0eb2a"

def mac_int(m): return int(m.replace(":", ""), 16)
def mac_str(v): return ":".join(f"{(v >> s) & 0xff:02X}" for s in range(40, -8, -8))

async def main():
    cli = APIClient(HOST, PORT, None)
    await cli.connect(login=True)
    info = await cli.device_info()
    print("device:", info.name, info.mac_address, "bt", info.bluetooth_mac_address, "flags", info.bluetooth_proxy_feature_flags, "api", cli.api_version)

    seen, count = {}, 0
    def on_adv(resp):
        nonlocal count
        for a in resp.advertisements:
            count += 1
            seen[a.address] = a.rssi
    unsub = cli.subscribe_bluetooth_le_raw_advertisements(on_adv)
    cli.subscribe_bluetooth_connections_free(lambda f, l, a: print(f"connections free {f}/{l} {[mac_str(x) for x in a]}"))
    await asyncio.sleep(10)
    print(f"adverts: {count} in 10 s, {len(seen)} devices; eQ-3:", {mac_str(k): v for k, v in seen.items() if mac_str(k).startswith("00:1A:22")})

    eq3 = sorted(((v, k) for k, v in seen.items() if mac_str(k).startswith("00:1A:22")), reverse=True)
    addr = mac_int(TARGET) if TARGET else eq3[0][1]
    print("target", mac_str(addr))
    state = {}
    def on_state(connected, mtu, error):
        print(f"connection state: connected={connected} mtu={mtu} error={error}")
        state.update(connected=connected)
    t = time.time()
    await cli.bluetooth_device_connect(addr, on_state, timeout=30, feature_flags=info.bluetooth_proxy_feature_flags, has_cache=False, address_type=0)
    if not state.get("connected"):
        sys.exit("FAIL: not connected")
    print(f"connected in {time.time() - t:.1f} s")
    svcs = await cli.bluetooth_gatt_get_services(addr)
    chars = {}
    for s in svcs.services:
        print("service", s.uuid, hex(s.handle))
        for c in s.characteristics:
            print("   char", c.uuid, hex(c.handle), "props", hex(c.properties), "descs", [(d.uuid, hex(d.handle)) for d in c.descriptors])
            chars[c.uuid] = c
    w, n = chars[EQ3_WRITE], chars[EQ3_NOTIFY]
    got = asyncio.get_running_loop().create_future()
    await cli.bluetooth_gatt_start_notify(addr, n.handle, lambda h, d: got.done() or got.set_result(bytes(d)))
    cccd = [d for d in n.descriptors if d.uuid.startswith("00002902")]
    if cccd:
        await cli.bluetooth_gatt_write_descriptor(addr, cccd[0].handle, b"\x01\x00")
    d = time.localtime()
    req = bytes([0x03, d.tm_year - 2000, d.tm_mon, d.tm_mday, d.tm_hour, d.tm_min, d.tm_sec])
    await cli.bluetooth_gatt_write(addr, w.handle, req, True)
    status = await asyncio.wait_for(got, 10)
    print(f"STATUS {status.hex()} mode=0x{status[2]:x} valve={status[3]}% target={status[5] / 2}°C")
    name = chars.get("00002a00-0000-1000-8000-00805f9b34fb")
    if name:
        print("device name:", bytes(await cli.bluetooth_gatt_read(addr, name.handle)))
    await cli.bluetooth_device_disconnect(addr)
    print("disconnected; PASS")
    unsub()
    await cli.disconnect()

asyncio.run(main())
