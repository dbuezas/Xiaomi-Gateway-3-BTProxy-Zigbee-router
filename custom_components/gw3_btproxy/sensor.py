"""Sensor: state of the gateway's Zigbee router (off, up, error)."""

from __future__ import annotations

from homeassistant.components.sensor import SensorDeviceClass, SensorEntity
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback

from . import GatewayConfigEntry
from .coordinator import ROUTER_ERROR, ROUTER_OFF, ROUTER_UP
from .entity import GatewayEntity


async def async_setup_entry(hass: HomeAssistant, entry: GatewayConfigEntry, async_add_entities: AddEntitiesCallback) -> None:
    async_add_entities([ZigbeeRouterSensor(entry.runtime_data, "zigbee_router")])


class ZigbeeRouterSensor(GatewayEntity, SensorEntity):
    _attr_icon = "mdi:router-network"
    _attr_device_class = SensorDeviceClass.ENUM
    _attr_options = [ROUTER_OFF, ROUTER_UP, ROUTER_ERROR]

    @property
    def native_value(self) -> str | None:
        return self.coordinator.data and self.coordinator.data["router"]
