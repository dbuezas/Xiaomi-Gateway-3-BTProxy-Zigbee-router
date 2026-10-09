"""Constants for the Xiaomi Gateway 3 BT proxy integration."""

DOMAIN = "gw3_btproxy"

CONF_ZIGBEE_ROUTER = "zigbee_router"  # option: keep the gateway's Zigbee chip running as a router

API_PORT = 6053  # ESPHome native API of gw3-btproxy
ZIGBEE_PORT = 8888  # openmiio_agent serves the Zigbee chip here in ZHA mode

# Only this model is tested: the proxy, the GPIO31 chip reset and the boot hook are specific to its hardware
# and firmware. Other gateways the Xiaomi Gateway 3 integration supports may differ, so they are refused.
SUPPORTED_MODELS = ("lumi.gateway.mgl03",)
# Gateway firmwares tested on the device (proxy, switch script, boot hook, Zigbee router). Others are refused:
# each firmware generation changes how Xiaomi's Bluetooth app is supervised and how the chip is reset.
SUPPORTED_FIRMWARES = ("1.5.0_0026", "1.5.0_0102", "1.5.1_0032", "1.5.4_0090", "1.5.7_0001")

GW_DIR = "/data"
GW_FILES = ("gw3-btproxy", "gw3-btproxy.sh")  # installed from ./bin into GW_DIR

UPDATE_INTERVAL = 120  # seconds
