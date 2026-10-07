"""Run the gateway's Zigbee chip (EmberZNet NCP, EZSP) as a router in the ZHA network, via bellows.

Connecting to the chip always resets it. Once joined, it routes on its own; after a reset it needs a
networkInit, with stack profile 2 and security level 5 set first (otherwise it answers NOT_JOINED).
"""

from __future__ import annotations

import asyncio
import glob
import json
import logging
import os
import socket
import sqlite3
import time
from dataclasses import dataclass
from urllib.parse import urlparse

from homeassistant.core import HomeAssistant

from .const import ZIGBEE_PORT

_LOGGER = logging.getLogger(__name__)


@dataclass
class Network:
    channel: int
    pan_id: int
    extended_pan_id: str


class ZigbeeError(Exception):
    """The chip did not do what was asked."""


# One lock per gateway, shared by every coordinator instance: a config entry reload must not let a new
# coordinator open the chip while the old one is still using it.
_LOCKS: dict[str, asyncio.Lock] = {}


def chip_lock(host: str) -> asyncio.Lock:
    return _LOCKS.setdefault(host, asyncio.Lock())


async def zha_network(hass: HomeAssistant) -> Network:
    """Channel, PAN ID and extended PAN ID of the running ZHA network."""
    try:
        from homeassistant.components.zha.helpers import get_zha_gateway

        info = get_zha_gateway(hass).application_controller.state.network_info
        return Network(int(info.channel), int(info.pan_id), str(info.extended_pan_id))
    except Exception as err:  # noqa: BLE001  ZHA internals change between releases
        _LOGGER.debug("ZHA gateway API unavailable (%r), reading the ZHA network backup", err)
    return await hass.async_add_executor_job(_zha_network_from_backup, hass.config.path("zigbee.db"))


def _zha_network_from_backup(path: str) -> Network:
    db = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        tables = [r[0] for r in db.execute("select name from sqlite_master where name like 'network_backups_v%'")]
        if not tables:
            raise ZigbeeError("ZHA is not set up")
        table = max(tables, key=lambda n: int(n.rsplit("_v", 1)[1]))
        (raw,) = db.execute(f"select backup_json from {table} order by id desc limit 1").fetchone()  # noqa: S608
    finally:
        db.close()
    info = json.loads(raw)["network_info"]
    return Network(int(info["channel"]), int(info["pan_id"], 16), info["extended_pan_id"])


def _ezsp(host: str):
    from bellows.ezsp import EZSP

    return EZSP({"path": f"socket://{host}:{ZIGBEE_PORT}", "baudrate": 115200, "flow_control": None})


async def _configure(ezsp) -> None:
    import bellows.types as t

    for cid, val in ((t.EzspConfigId.CONFIG_STACK_PROFILE, 2), (t.EzspConfigId.CONFIG_SECURITY_LEVEL, 5)):
        (status,) = await ezsp.setConfigurationValue(cid, val)
        if status != t.EzspStatus.SUCCESS:
            raise ZigbeeError(f"setConfigurationValue {cid.name}: {status}")


async def _wait_up(ezsp, start, timeout: float = 90) -> None:
    import bellows.types as t

    with ezsp.wait_for_stack_status(t.sl_Status.NETWORK_UP) as up:
        (status,) = await start()
        if status != t.EmberStatus.SUCCESS:
            raise ZigbeeError(f"chip refused: {status}")
        try:
            await asyncio.wait_for(up, timeout)
        except TimeoutError:
            (state,) = await ezsp.networkState()
            raise ZigbeeError(f"no NETWORK_UP within {timeout:.0f} s (network state {state})") from None


def same_network(a: Network, b: Network) -> bool:
    return a.channel == b.channel and a.pan_id == b.pan_id and a.extended_pan_id.lower() == b.extended_pan_id.lower()


@dataclass
class Chip:
    """What the chip holds: its IEEE address, and the network it has stored (None: no network)."""

    ieee: str
    node_type: str | None = None  # "COORDINATOR", "ROUTER", ...
    network: Network | None = None

    def is_router_of(self, net: Network) -> bool:
        return self.node_type == "ROUTER" and self.network is not None and same_network(self.network, net)


async def _init_stored(ezsp) -> Chip:
    """Bring up whatever network the chip has stored, and describe it."""
    import bellows.types as t

    (ieee,) = await ezsp.getEui64()
    chip = Chip(str(ieee))
    with ezsp.wait_for_stack_status(t.sl_Status.NETWORK_UP) as up:
        (status,) = await ezsp.networkInit(networkInitBitmask=t.EmberNetworkInitBitmask(0))
        if getattr(status, "name", "") == "NOT_JOINED":
            return chip  # nothing stored
        if status != t.EmberStatus.SUCCESS:
            raise ZigbeeError(f"networkInit: {status}")
        try:
            await asyncio.wait_for(up, 30)
        except TimeoutError:
            raise ZigbeeError("the stored network did not come up within 30 s") from None
    _status, node_type, params = await ezsp.getNetworkParameters()
    chip.node_type = getattr(node_type, "name", str(node_type))
    chip.network = Network(int(params.radioChannel), int(params.panId), str(params.extendedPanId))
    return chip


async def probe(host: str) -> Chip:
    """Bring the stored network up again (after any reset: this is also how the router is resumed) and describe it."""
    ezsp = _ezsp(host)
    try:
        await ezsp.connect(use_thread=False)
        await _configure(ezsp)
        return await _init_stored(ezsp)
    finally:
        await ezsp.disconnect()


async def reset(host: str) -> None:
    """Reset the chip: every connection to it does. Stops whatever network the last probe brought up."""
    ezsp = _ezsp(host)
    try:
        await ezsp.connect(use_thread=False)
    finally:
        await ezsp.disconnect()


async def leave(host: str, net: Network | None) -> bool:
    """Leave `net` (None: any network) if the chip is a router in it: bring it up first so the chip can
    announce its leave. Returns False (and changes nothing) when the chip is not such a router."""
    ezsp = _ezsp(host)
    try:
        await ezsp.connect(use_thread=False)
        await _configure(ezsp)
        chip = await _init_stored(ezsp)
        if not (chip.is_router_of(net) if net else chip.node_type == "ROUTER"):
            return False
        await ezsp.leaveNetwork(timeout=15)  # raises unless the chip confirms it, and waits for NETWORK_DOWN
        return True
    finally:
        await ezsp.disconnect()


async def zha_uses_chip(hass: HomeAssistant, host: str) -> bool:
    """ZHA's own radio is this chip (Xiaomi Gateway 3 integration's ZHA mode used as ZHA's coordinator)."""
    for entry in hass.config_entries.async_entries("zha"):
        url = urlparse(str((entry.data.get("device") or {}).get("path", "")))
        try:
            if url.port != ZIGBEE_PORT or not url.hostname:
                continue
        except ValueError:
            continue
        if url.hostname == host or await hass.async_add_executor_job(_same_host, url.hostname, host):
            return True
    return False


def _same_host(a: str, b: str) -> bool:
    try:
        return socket.gethostbyname(a) == socket.gethostbyname(b)
    except OSError:
        return False


# ---------- backup / restore of the chip's own (Xiaomi) network ----------
# Before the chip first joins ZHA it is the coordinator of the gateway's own Zigbee network. A zigpy backup of that
# network (open coordinator format, with the network key) is saved as a file; switching the router off writes the
# newest backup back, so the chip is the coordinator of its old network again.


def backup_dir(hass: HomeAssistant) -> str:
    return hass.config.path(".storage", "gw3_btproxy")  # .storage: not in git, and the files hold network keys


def _app(host: str):
    from bellows.zigbee.application import ControllerApplication

    return ControllerApplication({
        "device": {"path": f"socket://{host}:{ZIGBEE_PORT}", "baudrate": 115200},
        "backup_enabled": False,
        "startup_energy_scan": False,
        "database_path": None,
        "use_thread": False,
    })


async def backup(host: str, directory: str, chip: Chip) -> str:
    """Save a backup of the network the chip coordinates (its own Xiaomi one). Raises unless the backup is complete."""
    app = _app(host)
    try:
        await app.connect()
        await app.load_network_info(load_devices=True)
        backup = await app.backups.create_backup(load_devices=True)
    finally:
        await app.shutdown()
    info = backup.network_info
    if str(backup.node_info.ieee) != chip.ieee or chip.network is None or int(info.pan_id) != chip.network.pan_id:
        raise ZigbeeError("the backup does not match the chip's network")
    if bytes(info.network_key.key) in (bytes(16), b"\xff" * 16):
        raise ZigbeeError("the backup has no network key")
    name = os.path.join(directory, "zigbee-%s-%s.json" % (_ieee_tag(chip.ieee), time.strftime("%Y%m%d-%H%M%S")))
    obj = backup.as_open_coordinator_json()
    await asyncio.get_running_loop().run_in_executor(None, _write_json, name, obj)
    import zigpy.backups

    back = await asyncio.get_running_loop().run_in_executor(None, _read_json, name)
    if back != json.loads(json.dumps(obj)) or not zigpy.backups.NetworkBackup.from_dict(back).is_complete():
        raise ZigbeeError(f"the backup file {name} did not read back complete")
    _LOGGER.info("Backed up the gateway's Zigbee network to %s", name)
    return name


def _ieee_tag(ieee: str) -> str:
    return ieee.replace(":", "").lower()


def _write_json(path: str, obj) -> None:
    """Write atomically, readable by the owner only (the backups hold the network key)."""
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    os.chmod(os.path.dirname(path), 0o700)
    tmp = path + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.fchmod(fd, 0o600)  # also when a stale .tmp existed
    with os.fdopen(fd, "w") as f:
        json.dump(obj, f, indent=1)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def _read_json(path: str):
    with open(path) as f:
        return json.load(f)


def newest_backup(directory: str, ieee: str) -> str | None:  # call from an executor (file system)
    """The newest readable backup of this chip's own network."""
    for path in sorted(glob.glob(os.path.join(directory, f"zigbee-{_ieee_tag(ieee)}-*.json")), reverse=True):
        try:
            _read_json(path)
            return path
        except (OSError, ValueError) as err:
            _LOGGER.warning("Skipping the unreadable Zigbee backup %s (%r)", path, err)
    return None


async def restore(host: str, path: str) -> None:
    """Write a backup into the chip: it becomes the coordinator of that network again."""
    import zigpy.backups

    backup = zigpy.backups.NetworkBackup.from_dict(await asyncio.get_running_loop().run_in_executor(None, _read_json, path))
    app = _app(host)
    try:
        await app.connect()
        await app.backups.restore_backup(backup, counter_increment=5000)
        _LOGGER.info("Restored the gateway's Zigbee network from %s", path)
    finally:
        await app.shutdown()


async def join(host: str, net: Network) -> int:
    """Leave whatever network the chip has and join `net` as a router. The coordinator must permit joining."""
    import bellows.types as t

    ezsp = _ezsp(host)
    try:
        await ezsp.connect(use_thread=False)
        await _configure(ezsp)
        try:
            await ezsp.leaveNetwork()
        except Exception:  # noqa: BLE001  not joined to anything
            pass
        bits = t.EmberInitialSecurityBitmask
        (status,) = await ezsp.setInitialSecurityState(
            t.EmberInitialSecurityState(
                bitmask=bits.TRUST_CENTER_GLOBAL_LINK_KEY | bits.HAVE_PRECONFIGURED_KEY | bits.REQUIRE_ENCRYPTED_KEY,
                preconfiguredKey=t.KeyData(b"ZigBeeAlliance09"),  # the well-known default trust center link key
                networkKey=t.KeyData(bytes(16)),
                networkKeySequenceNumber=0,
                preconfiguredTrustCenterEui64=t.EUI64([0] * 8),
            )
        )
        if status != t.EmberStatus.SUCCESS:
            raise ZigbeeError(f"setInitialSecurityState: {status}")
        params = t.EmberNetworkParameters(
            extendedPanId=t.ExtendedPanId.convert(net.extended_pan_id),
            panId=t.EmberPanId(net.pan_id),
            radioTxPower=8,
            radioChannel=net.channel,
            joinMethod=t.EmberJoinMethod.USE_MAC_ASSOCIATION,
            nwkManagerId=t.EmberNodeId(0),
            nwkUpdateId=0,
            channels=t.Channels.from_channel_list([net.channel]),
        )
        await _wait_up(ezsp, lambda: ezsp.joinNetwork(t.EmberNodeType.ROUTER, params))
        (nwk,) = await ezsp.getNodeId()
        return int(nwk)
    finally:
        await ezsp.disconnect()
