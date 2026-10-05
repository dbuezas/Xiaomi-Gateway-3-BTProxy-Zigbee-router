"""Run the gateway's Zigbee chip (EmberZNet NCP, EZSP) as a router in the ZHA network, via bellows.

Connecting to the chip always resets it. Once joined, it routes on its own; after a reset it needs a
networkInit, with stack profile 2 and security level 5 set first (otherwise it answers NOT_JOINED).
"""

from __future__ import annotations

import asyncio
import json
import logging
import sqlite3
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


def zha_network(hass: HomeAssistant) -> Network:
    """Channel, PAN ID and extended PAN ID of the running ZHA network."""
    try:
        from homeassistant.components.zha.helpers import get_zha_gateway

        info = get_zha_gateway(hass).application_controller.state.network_info
        return Network(int(info.channel), int(info.pan_id), str(info.extended_pan_id))
    except Exception as err:  # noqa: BLE001  ZHA internals change between releases
        _LOGGER.debug("ZHA gateway API unavailable (%r), reading the ZHA network backup", err)
    db = sqlite3.connect(f"file:{hass.config.path('zigbee.db')}?mode=ro", uri=True)
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


async def resume(host: str) -> None:
    """Bring the stored router network up again (after any reset)."""
    import bellows.types as t

    ezsp = await _connect(host)
    try:
        await _configure(ezsp)
        await _wait_up(ezsp, lambda: ezsp.networkInit(networkInitBitmask=t.EmberNetworkInitBitmask(0)))
    finally:
        await ezsp.disconnect()


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
