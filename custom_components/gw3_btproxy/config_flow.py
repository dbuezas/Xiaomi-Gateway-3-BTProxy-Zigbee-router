"""Config flow: the gateway's IP address; telnet must be open (Xiaomi Gateway 3 integration)."""

from __future__ import annotations

from typing import Any

import voluptuous as vol

from homeassistant.config_entries import ConfigFlow, ConfigFlowResult
from homeassistant.const import CONF_HOST

from .const import DOMAIN
from .gateway import Gateway, GatewayError


class Gw3BtProxyConfigFlow(ConfigFlow, domain=DOMAIN):
    VERSION = 1

    async def async_step_user(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        errors: dict[str, str] = {}
        if user_input is not None:
            host = user_input[CONF_HOST].strip()
            try:
                mac = await Gateway(host).mac()
            except GatewayError:
                errors["base"] = "cannot_connect"
            else:
                if not mac:
                    errors["base"] = "not_gateway"
                else:
                    await self.async_set_unique_id(mac)
                    self._abort_if_unique_id_configured(updates={CONF_HOST: host})
                    return self.async_create_entry(title=f"Xiaomi Gateway 3 ({host})", data={CONF_HOST: host})
        return self.async_show_form(
            step_id="user",
            data_schema=vol.Schema({vol.Required(CONF_HOST, default=self._suggested_host()): str}),
            errors=errors,
        )

    def _suggested_host(self) -> str:
        """Offer the host of an existing Xiaomi Gateway 3 integration entry."""
        for entry in self.hass.config_entries.async_entries("xiaomi_gateway3"):
            host = entry.options.get("host") or entry.data.get("host")
            if host:
                return host
        return ""
