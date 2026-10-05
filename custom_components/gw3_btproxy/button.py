"""Button: join the gateway's Zigbee chip to the ZHA network as a router."""

from __future__ import annotations

import logging

from homeassistant.components.button import ButtonEntity
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from . import GatewayConfigEntry, zigbee
from .const import CONF_ZIGBEE_ROUTER
from .entity import GatewayEntity

_LOGGER = logging.getLogger(__name__)

PERMIT_SECONDS = 120


async def async_setup_entry(hass: HomeAssistant, entry: GatewayConfigEntry, async_add_entities: AddEntitiesCallback) -> None:
    async_add_entities([JoinZigbeeButton(entry.runtime_data, "join_zigbee")])


class JoinZigbeeButton(GatewayEntity, ButtonEntity):
    _attr_icon = "mdi:zigbee"

    async def async_press(self) -> None:
        coordinator = self.coordinator
        gateway = coordinator.gateway
        if not self.hass.services.has_service("zha", "permit"):
            raise HomeAssistantError("ZHA is not set up")
        if not await gateway.zigbee_tcp():
            raise HomeAssistantError(
                "The gateway's Zigbee chip is not on TCP 8888. Set the Xiaomi Gateway 3 integration's Zigbee mode to ZHA."
            )
        try:
            net = await self.hass.async_add_executor_job(zigbee.zha_network, self.hass)
            await self.hass.services.async_call("zha", "permit", {"duration": PERMIT_SECONDS}, blocking=True)
            nwk = await zigbee.join(gateway.host, net)
            coordinator.router_joined(await gateway.boot_id())
        except Exception as err:  # noqa: BLE001
            raise HomeAssistantError(f"Joining the Zigbee network failed: {err}") from err
        _LOGGER.info("Gateway Zigbee chip joined channel %s PAN 0x%04X as router 0x%04X", net.channel, net.pan_id, nwk)
        self.hass.config_entries.async_update_entry(
            coordinator.entry, options={**coordinator.entry.options, CONF_ZIGBEE_ROUTER: True}
        )
