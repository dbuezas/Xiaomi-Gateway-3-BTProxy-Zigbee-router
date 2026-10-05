"""Shared base entity: one device per gateway."""

from __future__ import annotations

from homeassistant.helpers.device_registry import CONNECTION_NETWORK_MAC, DeviceInfo
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from .const import DOMAIN
from .coordinator import GatewayCoordinator


class GatewayEntity(CoordinatorEntity[GatewayCoordinator]):
    _attr_has_entity_name = True

    def __init__(self, coordinator: GatewayCoordinator, key: str) -> None:
        super().__init__(coordinator)
        entry = coordinator.entry
        self._attr_unique_id = f"{entry.unique_id}_{key}"
        self._attr_translation_key = key
        self._attr_device_info = DeviceInfo(
            identifiers={(DOMAIN, entry.unique_id)},
            connections={(CONNECTION_NETWORK_MAC, entry.unique_id)},
            name="Xiaomi Gateway 3 radios",
            manufacturer="Xiaomi",
            model="ZNDMWG03LM",
        )
