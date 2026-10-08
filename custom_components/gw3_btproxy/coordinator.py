"""Polls the gateway: Bluetooth mode, and keeps the Zigbee router up after gateway reboots.

Every connection to the Zigbee chip resets it, so all of them (poll, switch on, switch off) go through one lock."""

from __future__ import annotations

import asyncio
import logging
import time
from datetime import timedelta

from homeassistant.components import persistent_notification
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers.storage import Store
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed

from . import zigbee
from .const import CONF_ZIGBEE_ROUTER, DOMAIN, UPDATE_INTERVAL
from .gateway import Gateway, GatewayError

_LOGGER = logging.getLogger(__name__)

ROUTER_OFF = "off"
ROUTER_UP = "up"
ROUTER_ERROR = "error"

# A hung connection to the Zigbee chip must not block every later poll (it did, for 21 hours).
CHIP_TIMEOUT = 90
PERMIT_SECONDS = 120
RETRY_MAX = 3600  # failed router restores are retried after 4, 8, 16 ... minutes, at most every hour


def _reason(err: BaseException) -> str:
    if isinstance(err, TimeoutError):
        return "the Zigbee chip did not answer in time"
    return repr(err)


class GatewayCoordinator(DataUpdateCoordinator[dict]):
    def __init__(self, hass: HomeAssistant, entry: ConfigEntry, gateway: Gateway) -> None:
        super().__init__(
            hass, _LOGGER, config_entry=entry, name=DOMAIN, update_interval=timedelta(seconds=UPDATE_INTERVAL)
        )
        self.entry = entry
        self.gateway = gateway
        self.zigbee_lock = zigbee.chip_lock(gateway.host)
        # Boot id at the last successful resume/join, kept across HA restarts: while it matches, the router is
        # up and the chip is left alone (every connection to it resets it). Cleared before each connection.
        self._store: Store[dict] = Store(hass, 1, f"{DOMAIN}.{entry.entry_id}")
        self._router_boot_id: str | None = None
        self._router_state = ROUTER_OFF
        self._router_fails = 0
        self._router_retry_at = 0.0
        self._router_left = False  # the chip left ZHA but its own network was not restored yet
        self._busy = False  # this instance's switch action holds the chip lock

    async def async_load(self) -> None:
        data = await self._store.async_load() or {}
        self._router_boot_id = data.get("router_boot_id")
        if self._router_boot_id:
            self._router_state = ROUTER_UP

    async def _set_boot_id(self, boot_id: str | None) -> None:
        self._router_boot_id = boot_id
        await self._store.async_save({"router_boot_id": boot_id})

    @property
    def router_option(self) -> bool:
        return bool(self.entry.options.get(CONF_ZIGBEE_ROUTER))

    def _router_now(self) -> str:
        if not self.router_option:
            return ROUTER_OFF
        return ROUTER_ERROR if self._router_left else self._router_state

    def _push_router_state(self) -> None:
        """Show the result of a switch action right away (the options listener only refreshes on a change)."""
        if self.data is not None:
            self.async_set_updated_data({**self.data, "router": self._router_now()})

    async def _async_update_data(self) -> dict:
        try:
            # read-only when the gateway boot hook restores the mode; without the hook, "status" restores it
            bt = await self.gateway.bt_mode("status")
        except GatewayError as err:
            raise UpdateFailed(str(err)) from err
        return {"bt": bt, "router": await self._ensure_router()}

    async def _ensure_router(self) -> str:
        if not self.router_option or self._router_left:
            return self._router_now()  # left ZHA, own network not restored: switching off again retries the restore
        if self.zigbee_lock.locked():
            if self._busy:
                return self._router_state  # this instance's switch action is using the chip
            # after a reload the old instance may still be using it
            return self._router_state if self._router_boot_id else ROUTER_ERROR
        if self._router_fails and time.monotonic() < self._router_retry_at:
            return ROUTER_ERROR
        async with self.zigbee_lock:
            try:
                boot_id = await self.gateway.boot_id()
                if boot_id != self._router_boot_id:
                    # gateway rebooted or openmiio_agent restarted (or no boot id stored yet): the chip may be reset
                    if await zigbee.zha_uses_chip(self.hass, self.gateway.host):
                        raise zigbee.ZigbeeError("ZHA uses this chip as its own radio")
                    _LOGGER.info("Resuming the Zigbee router on %s", self.gateway.host)
                    net = await zigbee.zha_network(self.hass)
                    await self._set_boot_id(None)
                    chip = await asyncio.wait_for(zigbee.probe(self.gateway.host), CHIP_TIMEOUT)
                    await self._refuse_zha_coordinator(chip, net)
                    if not chip.is_router_of(net):
                        raise zigbee.ZigbeeError(f"the chip is not a router in the ZHA network ({chip})")
                    await self._set_boot_id(boot_id)
                self._router_fails = 0
                self._router_state = ROUTER_UP
            except Exception as err:  # noqa: BLE001
                self._router_fails += 1
                delay = min(UPDATE_INTERVAL * 2**self._router_fails, RETRY_MAX)
                self._router_retry_at = time.monotonic() + delay
                _LOGGER.warning("Zigbee router on %s: %r (next try in %d min)", self.gateway.host, err, delay // 60)
                self._router_state = ROUTER_ERROR
        return self._router_state

    def _set_option(self, on: bool) -> None:
        self.hass.config_entries.async_update_entry(self.entry, options={**self.entry.options, CONF_ZIGBEE_ROUTER: on})

    async def _check_chip_reachable(self) -> None:
        hass, host = self.hass, self.gateway.host
        if await zigbee.zha_uses_chip(hass, host):
            raise HomeAssistantError("ZHA uses this chip as its own radio; it cannot also be a router")
        try:
            tcp = await self.gateway.zigbee_tcp()
        except GatewayError as err:
            raise HomeAssistantError(f"The gateway does not answer: {err}") from err
        if not tcp:
            raise HomeAssistantError(
                "The gateway's Zigbee chip is not on TCP 8888. Set the Xiaomi Gateway 3 integration's Zigbee mode to ZHA."
            )

    async def _refuse_zha_coordinator(self, chip: zigbee.Chip, net: zigbee.Network | None) -> None:
        """The chip holds ZHA's own network as coordinator (it once was ZHA's radio). The probe has just brought
        that duplicate coordinator up: reset it at once, then refuse."""
        if net and chip.network and zigbee.same_network(chip.network, net) and chip.node_type != "ROUTER":
            try:
                await asyncio.wait_for(zigbee.reset(self.gateway.host), CHIP_TIMEOUT)
            except Exception as err:  # noqa: BLE001  the refusal below is what matters
                _LOGGER.warning("Could not reset the chip: %r", err)
            raise HomeAssistantError(
                f"The chip holds the ZHA network as {chip.node_type} (a copy of ZHA's own network); it was reset, "
                "nothing else changed"
            )

    async def router_on(self) -> None:
        if self.zigbee_lock.locked():
            raise HomeAssistantError("The Zigbee chip is busy; try again in a minute")
        async with self.zigbee_lock:
            self._busy = True
            try:
                await self._check_chip_reachable()
                if not self.hass.services.has_service("zha", "permit"):
                    raise HomeAssistantError("ZHA is not set up")
                await self._join_locked()
            finally:
                self._busy = False
                self._push_router_state()

    async def _join_locked(self) -> None:
        hass, host = self.hass, self.gateway.host
        saved, joining = None, False
        try:
            net = await zigbee.zha_network(hass)
            await self._set_boot_id(None)
            chip = await asyncio.wait_for(zigbee.probe(host), CHIP_TIMEOUT)
            await self._refuse_zha_coordinator(chip, net)
            if not chip.is_router_of(net):
                if chip.node_type == "COORDINATOR":
                    # the gateway's own network: keep it, so switching off can give it back. No backup, no join:
                    # backup() raises unless the backup is complete and read back.
                    saved = await asyncio.wait_for(zigbee.backup(host, zigbee.backup_dir(hass), chip), CHIP_TIMEOUT)
                    _LOGGER.info("Saved the gateway's own Zigbee network before joining: %s", saved)
                elif chip.network:
                    _LOGGER.warning("The chip was a %s in another network (%s); it is not backed up", chip.node_type, chip.network)
                await hass.services.async_call("zha", "permit", {"duration": PERMIT_SECONDS}, blocking=True)
                joining = True  # from here the chip may have forgotten its own network
                try:
                    nwk = await asyncio.wait_for(zigbee.join(host, net), PERMIT_SECONDS + 30)
                finally:
                    try:
                        await hass.services.async_call("zha", "permit", {"duration": 0}, blocking=True)
                    except Exception as err:  # noqa: BLE001
                        _LOGGER.warning("Could not close ZHA pairing: %r", err)
                _LOGGER.info("Gateway Zigbee chip joined channel %s PAN 0x%04X as router 0x%04X", net.channel, net.pan_id, nwk)
        except BaseException as err:  # also a cancel (HA stopping) in the middle of the join
            joined = False
            if joining and not isinstance(err, asyncio.CancelledError):
                # the join may have completed after all (just as the timeout fired)
                try:
                    joined = (await asyncio.wait_for(zigbee.probe(host), CHIP_TIMEOUT)).is_router_of(net)
                except Exception as perr:  # noqa: BLE001  unknown: restore below
                    _LOGGER.warning("Could not check the chip after the failed join: %r", perr)
            if joining and saved and not joined:
                # the chip left its own network for the join: give that network back now
                try:
                    await asyncio.wait_for(zigbee.restore(host, saved), 120)
                    _LOGGER.warning("Joining failed; the gateway's own Zigbee network was restored from %s", saved)
                except Exception as rerr:  # noqa: BLE001
                    _LOGGER.error("Joining failed and restoring %s failed too: %r", saved, rerr)
            if not joined:
                if self.router_option:
                    self._router_state = ROUTER_ERROR  # the chip was touched: "up" is no longer known
                if isinstance(err, (HomeAssistantError, asyncio.CancelledError)):
                    raise
                raise HomeAssistantError(f"Turning the Zigbee router on failed: {_reason(err)}") from err
            _LOGGER.warning("The join reported %r but the chip is a router in the ZHA network: keeping it", err)
        # joined (or already a router): from here on, failures are only warnings
        self._router_fails, self._router_left, self._router_state = 0, False, ROUTER_UP
        self._set_option(True)
        try:
            await self._set_boot_id(await self.gateway.boot_id())
        except GatewayError as err:
            _LOGGER.warning("Could not read the gateway boot id (%r); the next poll checks the router", err)

    async def router_off(self) -> None:
        if self.zigbee_lock.locked():
            raise HomeAssistantError("The Zigbee chip is busy; try again in a minute")
        if not self.router_option:
            return  # already off: never write an old backup over the chip's current network
        async with self.zigbee_lock:
            self._busy = True
            try:
                await self._check_chip_reachable()
                await self._leave_locked()
            finally:
                self._busy = False
                self._push_router_state()

    async def _leave_locked(self) -> None:
        hass, host = self.hass, self.gateway.host
        try:
            try:
                net = await zigbee.zha_network(hass)
            except Exception as err:  # noqa: BLE001  ZHA removed: leave whatever network the router is in
                _LOGGER.warning("ZHA's network is unknown (%r); leaving any network the chip routes for", err)
                net = None
            await self._set_boot_id(None)
            chip = await asyncio.wait_for(zigbee.probe(host), CHIP_TIMEOUT)
            await self._refuse_zha_coordinator(chip, net)
            if chip.node_type == "ROUTER" and (net is None or chip.is_router_of(net)):
                if not await asyncio.wait_for(zigbee.leave(host, net), CHIP_TIMEOUT):
                    raise zigbee.ZigbeeError("the chip was no longer a router")
                self._router_left = True
            backup = await hass.async_add_executor_job(zigbee.newest_backup, zigbee.backup_dir(hass), chip.ieee)
            if backup:
                await asyncio.wait_for(zigbee.restore(host, backup), 120)
                message = (
                    f"The gateway's Zigbee chip has its own network back (from `{backup}`). To use it with "
                    "Xiaomi's app again, set the Xiaomi Gateway 3 integration's Zigbee mode back to Mi Home."
                )
            else:
                message = (
                    "The gateway's Zigbee chip left the ZHA network. There is no backup of its own network (it had "
                    "none when it first joined, or joined with version 0.2.x), so it has no network now: pair its "
                    "devices again in Mi Home."
                )
        except Exception as err:  # noqa: BLE001
            self._router_state = ROUTER_ERROR  # the chip was touched: "up" is no longer known
            if isinstance(err, HomeAssistantError):
                raise
            raise HomeAssistantError(f"Turning the Zigbee router off failed: {_reason(err)}. Try again.") from err
        persistent_notification.async_create(hass, message, title="Gateway Zigbee router off", notification_id="gw3_btproxy_zigbee_off")
        self._router_left, self._router_fails, self._router_state = False, 0, ROUTER_OFF
        self._set_option(False)
