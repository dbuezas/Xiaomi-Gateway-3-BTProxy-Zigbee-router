"""Shared base entity: one device per gateway."""

from __future__ import annotations

from homeassistant.helpers.device_registry import CONNECTION_NETWORK_MAC, CONNECTION_ZIGBEE, DeviceInfo
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import CONF_ZIGBEE_IEEE, DOMAIN
from .coordinator import GatewayCoordinator


class GatewayEntity(CoordinatorEntity[GatewayCoordinator]):
    _attr_has_entity_name = True

    def __init__(self, coordinator: GatewayCoordinator, key: str) -> None:
        super().__init__(coordinator)
        entry = coordinator.entry
        self._attr_unique_id = f"{entry.unique_id}_{key}"
        self._attr_translation_key = key
        # the same connections as the Xiaomi Gateway 3 integration's device (mac) and ZHA's (zigbee) link them
        connections = {(CONNECTION_NETWORK_MAC, entry.unique_id)}
        if ieee := entry.data.get(CONF_ZIGBEE_IEEE):
            connections.add((CONNECTION_ZIGBEE, ieee))
        self._attr_device_info = DeviceInfo(
            identifiers={(DOMAIN, entry.unique_id)},
            connections=connections,
            name="Xiaomi Gateway 3 radios",
            manufacturer="Xiaomi",
            model="ZNDMWG03LM",
        )
