"""Xiaomi Gateway 3: ESPHome-compatible Bluetooth proxy and Zigbee router."""

from __future__ import annotations

import logging

from homeassistant.config_entries import ConfigEntry
from homeassistant.const import CONF_HOST, Platform
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import ConfigEntryError, ConfigEntryNotReady
from homeassistant.helpers import entity_registry as er
from homeassistant.helpers.storage import Store

from .const import DOMAIN, SUPPORTED_MODELS
from .coordinator import GatewayCoordinator
from .gateway import Gateway, GatewayError, UnsupportedModel

_LOGGER = logging.getLogger(__name__)

PLATFORMS = [Platform.SWITCH, Platform.SENSOR]

type GatewayConfigEntry = ConfigEntry[GatewayCoordinator]


async def async_setup_entry(hass: HomeAssistant, entry: GatewayConfigEntry) -> bool:
    gateway = Gateway(entry.data[CONF_HOST])
    try:
        if await gateway.install():
            _LOGGER.info("Installed gw3-btproxy on %s", gateway.host)
    except UnsupportedModel as err:  # no retries: it will not become supported
        raise ConfigEntryError(f"Gateway model {err} is not supported (only {', '.join(SUPPORTED_MODELS)})") from err
    except GatewayError as err:
        raise ConfigEntryNotReady(str(err)) from err
    # the "Join Zigbee network as router" button (0.2.x) became the Zigbee router switch
    registry = er.async_get(hass)
    if old := registry.async_get_entity_id("button", DOMAIN, f"{entry.unique_id}_join_zigbee"):
        registry.async_remove(old)
    coordinator = GatewayCoordinator(hass, entry, gateway)
    await coordinator.async_load()
    await coordinator.async_config_entry_first_refresh()
    entry.runtime_data = coordinator
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    entry.async_on_unload(entry.add_update_listener(_options_updated))
    return True


async def _options_updated(hass: HomeAssistant, entry: GatewayConfigEntry) -> None:
    await entry.runtime_data.async_request_refresh()


async def async_unload_entry(hass: HomeAssistant, entry: GatewayConfigEntry) -> bool:
    return await hass.config_entries.async_unload_platforms(entry, PLATFORMS)


async def async_remove_entry(hass: HomeAssistant, entry: GatewayConfigEntry) -> None:
    await Store(hass, 1, f"{DOMAIN}.{entry.entry_id}").async_remove()
