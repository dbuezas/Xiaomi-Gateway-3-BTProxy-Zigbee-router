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
import sqlite3
import time
from dataclasses import dataclass

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


async def _connect(host: str):
    from bellows.ezsp import EZSP

    ezsp = EZSP({"path": f"socket://{host}:{ZIGBEE_PORT}", "baudrate": 115200, "flow_control": None})
    await ezsp.connect(use_thread=False)
    return ezsp


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


async def _stored_network(ezsp) -> Network:
    _status, _node_type, params = await ezsp.getNetworkParameters()
    return Network(int(params.radioChannel), int(params.panId), str(params.extendedPanId))


async def resume(host: str) -> Network:
    """Bring the stored router network up again (after any reset) and return it."""
    import bellows.types as t

    ezsp = await _connect(host)
    try:
        await _configure(ezsp)
        await _wait_up(ezsp, lambda: ezsp.networkInit(networkInitBitmask=t.EmberNetworkInitBitmask(0)))
        return await _stored_network(ezsp)
    finally:
        await ezsp.disconnect()


async def leave(host: str) -> None:
    """Leave the network properly: bring it up first so the chip can announce its leave, then leave."""
    import bellows.types as t

    ezsp = await _connect(host)
    try:
        await _configure(ezsp)
        try:
            await _wait_up(ezsp, lambda: ezsp.networkInit(networkInitBitmask=t.EmberNetworkInitBitmask(0)), timeout=30)
        except Exception as err:  # noqa: BLE001  nothing stored: nothing to leave
            _LOGGER.debug("No stored network to leave (%r)", err)
            return
        await ezsp.leaveNetwork()
    finally:
        await ezsp.disconnect()


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


async def backup_if_coordinator(host: str, directory: str) -> str | None:
    """Save a backup when the chip is the coordinator of a network (its own Xiaomi one). Returns the file name."""
    app = _app(host)
    try:
        await app.connect()
        try:
            await app.load_network_info(load_devices=True)
        except Exception as err:  # noqa: BLE001  no network formed
            _LOGGER.debug("No network to back up (%r)", err)
            return None
        if app.state.node_info.nwk != 0x0000:
            return None  # a router or end device: not the gateway's own network
        backup = await app.backups.create_backup(load_devices=True)
        name = os.path.join(directory, "zigbee-%s-%s.json" % (str(app.state.node_info.ieee).replace(":", ""), time.strftime("%Y%m%d-%H%M%S")))
        await asyncio.get_running_loop().run_in_executor(None, _write_json, name, backup.as_open_coordinator_json())
        _LOGGER.info("Backed up the gateway's Zigbee network to %s", name)
        return name
    finally:
        await app.shutdown()


def _write_json(path: str, obj) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        json.dump(obj, f, indent=1)


def _read_json(path: str):
    with open(path) as f:
        return json.load(f)


def newest_backup(directory: str) -> str | None:  # call from an executor (file system)
    files = sorted(glob.glob(os.path.join(directory, "zigbee-*.json")))
    return files[-1] if files else None


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

    ezsp = await _connect(host)
    try:
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
