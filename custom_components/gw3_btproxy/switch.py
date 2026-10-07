"""Switches: Bluetooth proxy (gw3-btproxy vs Xiaomi's BT app) and Zigbee router (ZHA router vs the gateway's own network)."""

from __future__ import annotations

import asyncio
import logging

from homeassistant.components import persistent_notification
from homeassistant.components.switch import SwitchEntity
from homeassistant.config_entries import SOURCE_USER
from homeassistant.const import CONF_HOST, CONF_PORT
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from . import GatewayConfigEntry, zigbee
from .const import API_PORT, CONF_ZIGBEE_ROUTER
from .entity import GatewayEntity
from .gateway import GatewayError

_LOGGER = logging.getLogger(__name__)


async def async_setup_entry(hass: HomeAssistant, entry: GatewayConfigEntry, async_add_entities: AddEntitiesCallback) -> None:
    async_add_entities([BluetoothProxySwitch(entry.runtime_data, "bluetooth_proxy"), ZigbeeRouterSwitch(entry.runtime_data, "zigbee_router")])


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


PERMIT_SECONDS = 120


class ZigbeeRouterSwitch(GatewayEntity, SwitchEntity):
    """On: the gateway's Zigbee chip is a router in the ZHA network, restored after reboots.
    Off: it leaves the ZHA network and gets its own (Xiaomi) network back from the backup taken before the first join."""

    _attr_icon = "mdi:router-network"

    @property
    def is_on(self) -> bool:
        return bool(self.coordinator.entry.options.get(CONF_ZIGBEE_ROUTER))

    async def async_turn_on(self, **kwargs) -> None:
        coordinator, gateway, hass = self.coordinator, self.coordinator.gateway, self.hass
        if not hass.services.has_service("zha", "permit"):
            raise HomeAssistantError("ZHA is not set up")
        if not await gateway.zigbee_tcp():
            raise HomeAssistantError(
                "The gateway's Zigbee chip is not on TCP 8888. Set the Xiaomi Gateway 3 integration's Zigbee mode to ZHA."
            )
        try:
            net = await zigbee.zha_network(hass)
            # keep the gateway's own network, so switching off can give it back
            saved = await asyncio.wait_for(zigbee.backup_if_coordinator(gateway.host, zigbee.backup_dir(hass)), 90)
            if saved:
                _LOGGER.info("Saved the gateway's own Zigbee network before joining: %s", saved)
            # the chip may still hold this ZHA network (switched off and on again without restoring): resume is enough
            try:
                stored = await asyncio.wait_for(zigbee.resume(gateway.host), 90)
            except Exception:  # noqa: BLE001
                stored = None
            if not (stored and zigbee.same_network(stored, net)):
                await hass.services.async_call("zha", "permit", {"duration": PERMIT_SECONDS}, blocking=True)
                nwk = await asyncio.wait_for(zigbee.join(gateway.host, net), PERMIT_SECONDS + 30)
                _LOGGER.info("Gateway Zigbee chip joined channel %s PAN 0x%04X as router 0x%04X", net.channel, net.pan_id, nwk)
            coordinator.router_joined(await gateway.boot_id())
        except HomeAssistantError:
            raise
        except Exception as err:  # noqa: BLE001
            raise HomeAssistantError(f"Turning the Zigbee router on failed: {err!r}") from err
        hass.config_entries.async_update_entry(coordinator.entry, options={**coordinator.entry.options, CONF_ZIGBEE_ROUTER: True})
        await coordinator.async_request_refresh()

    async def async_turn_off(self, **kwargs) -> None:
        coordinator, gateway, hass = self.coordinator, self.coordinator.gateway, self.hass
        try:
            await asyncio.wait_for(zigbee.leave(gateway.host), 90)
            backup = await hass.async_add_executor_job(zigbee.newest_backup, zigbee.backup_dir(hass))
            if backup:
                await asyncio.wait_for(zigbee.restore(gateway.host, backup), 120)
                persistent_notification.async_create(
                    hass,
                    "The gateway's Zigbee chip left the ZHA network and has its own network back "
                    f"(from `{backup}`). To use it with Xiaomi's app again, set the Xiaomi Gateway 3 integration's "
                    "Zigbee mode back to Mi Home.",
                    title="Gateway Zigbee router off",
                    notification_id="gw3_btproxy_zigbee_off",
                )
            else:
                _LOGGER.warning("No backup of the gateway's own Zigbee network; the chip now has no network")
        except Exception as err:  # noqa: BLE001
            raise HomeAssistantError(f"Turning the Zigbee router off failed: {err!r}") from err
        hass.config_entries.async_update_entry(coordinator.entry, options={**coordinator.entry.options, CONF_ZIGBEE_ROUTER: False})
        await coordinator.async_request_refresh()
