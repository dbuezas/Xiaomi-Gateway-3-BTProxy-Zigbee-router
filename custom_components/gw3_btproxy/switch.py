"""Bluetooth proxy switch: on = gw3-btproxy owns the BT chip, off = Xiaomi's own BT app."""

from __future__ import annotations

import asyncio
import logging

from homeassistant.components.switch import SwitchEntity
from homeassistant.config_entries import SOURCE_USER
from homeassistant.const import CONF_HOST, CONF_PORT
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from . import GatewayConfigEntry
from .const import API_PORT
from .entity import GatewayEntity
from .gateway import GatewayError

_LOGGER = logging.getLogger(__name__)


async def async_setup_entry(hass: HomeAssistant, entry: GatewayConfigEntry, async_add_entities: AddEntitiesCallback) -> None:
    async_add_entities([BluetoothProxySwitch(entry.runtime_data, "bluetooth_proxy")])


class BluetoothProxySwitch(GatewayEntity, SwitchEntity):
    _attr_icon = "mdi:bluetooth-connect"

    @property
    def is_on(self) -> bool:
        return bool(self.coordinator.data and self.coordinator.data["bt"])

    async def async_turn_on(self, **kwargs) -> None:
        await self._set("on")
        await self._ensure_esphome_entry()

    async def async_turn_off(self, **kwargs) -> None:
        await self._set("off")

    async def _set(self, action: str) -> None:
        try:
            await self.coordinator.gateway.bt_mode(action)
        except GatewayError as err:
            raise HomeAssistantError(str(err)) from err
        await self.coordinator.async_request_refresh()

    async def _ensure_esphome_entry(self) -> None:
        """Add the proxy to the ESPHome integration, once."""
        host = self.coordinator.gateway.host
        for entry in self.hass.config_entries.async_entries("esphome"):
            if entry.data.get(CONF_HOST) == host and entry.data.get(CONF_PORT, API_PORT) == API_PORT:
                return
        for _ in range(10):  # the proxy needs a few seconds to open its API port
            result = await self.hass.config_entries.flow.async_init("esphome", context={"source": SOURCE_USER})
            result = await self.hass.config_entries.flow.async_configure(
                result["flow_id"], {CONF_HOST: host, CONF_PORT: API_PORT}
            )
            if result["type"] == "create_entry":
                _LOGGER.info("Added the gateway BT proxy to ESPHome")
                return
            if result["type"] == "form":
                self.hass.config_entries.flow.async_abort(result["flow_id"])
            await asyncio.sleep(3)
        _LOGGER.warning("Could not add %s:%s to ESPHome; add it by hand", host, API_PORT)
