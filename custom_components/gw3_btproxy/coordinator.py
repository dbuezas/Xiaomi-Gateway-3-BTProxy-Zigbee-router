"""Polls the gateway: Bluetooth mode, and keeps the Zigbee router up after gateway reboots."""

from __future__ import annotations

import asyncio
import logging
from datetime import timedelta

from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.update_coordinator import DataUpdateCoordinator, UpdateFailed

from . import zigbee
from .const import CONF_ZIGBEE_ROUTER, DOMAIN, UPDATE_INTERVAL
from .gateway import Gateway, GatewayError

_LOGGER = logging.getLogger(__name__)

ROUTER_OFF = "off"
ROUTER_UP = "up"
ROUTER_ERROR = "error"

# A hung connection to the Zigbee chip must not block every later poll (it did, for 21 hours).
RESUME_TIMEOUT = 90


class GatewayCoordinator(DataUpdateCoordinator[dict]):
    def __init__(self, hass: HomeAssistant, entry: ConfigEntry, gateway: Gateway) -> None:
        super().__init__(hass, _LOGGER, name=DOMAIN, update_interval=timedelta(seconds=UPDATE_INTERVAL))
        self.entry = entry
        self.gateway = gateway
        self._router_boot_id: str | None = None  # boot id at the last successful resume/join

    async def _async_update_data(self) -> dict:
        try:
            # read-only when the gateway boot hook restores the mode; without the hook, "status" restores it
            bt = await self.gateway.bt_mode("status")
        except GatewayError as err:
            raise UpdateFailed(str(err)) from err
        return {"bt": bt, "router": await self._ensure_router()}

    async def _ensure_router(self) -> str:
        if not self.entry.options.get(CONF_ZIGBEE_ROUTER):
            return ROUTER_OFF
        try:
            boot_id = await self.gateway.boot_id()
            if boot_id != self._router_boot_id:
                # gateway rebooted or openmiio_agent restarted (or HA started): the chip may be reset
                _LOGGER.info("Resuming the Zigbee router on %s", self.gateway.host)
                await asyncio.wait_for(zigbee.resume(self.gateway.host), RESUME_TIMEOUT)
                self._router_boot_id = boot_id
        except Exception as err:  # noqa: BLE001
            _LOGGER.warning("Zigbee router on %s: %r", self.gateway.host, err)
            return ROUTER_ERROR
        return ROUTER_UP

    def router_joined(self, boot_id: str) -> None:
        self._router_boot_id = boot_id
