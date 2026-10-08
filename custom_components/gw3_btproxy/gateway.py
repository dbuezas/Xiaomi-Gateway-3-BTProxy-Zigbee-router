"""Talk to the Xiaomi Gateway 3 over telnet and install gw3-btproxy on it."""

from __future__ import annotations

import asyncio
import hashlib
import logging
import re
import socket
from pathlib import Path

from aiohttp import web

from .const import GW_DIR, GW_FILES

_LOGGER = logging.getLogger(__name__)

BIN_DIR = Path(__file__).parent / "bin"
START, DONE = "__GW3_START__", "__GW3_DONE__"


class GatewayError(Exception):
    """Telnet to the gateway failed."""


class Gateway:
    """One telnet session per command; the gateway's telnetd logs in as admin without password."""

    def __init__(self, host: str) -> None:
        self.host = host
        self._lock = asyncio.Lock()

    async def run(self, cmd: str, timeout: float = 30) -> str:
        """Run a shell command and return its output."""
        async with self._lock:
            try:
                return await asyncio.wait_for(self._run(cmd), timeout)
            except (OSError, asyncio.TimeoutError) as err:
                raise GatewayError(f"telnet {self.host}: {err!r}") from err

    async def _run(self, cmd: str) -> str:
        reader, writer = await asyncio.open_connection(self.host, 23)
        try:
            buf = b""
            sent_login = sent_cmd = False
            while True:
                data = await reader.read(4096)
                if not data:
                    raise GatewayError("telnet closed")
                data = self._negotiate(data, writer)
                buf += data
                text = buf.decode("latin1")
                if not sent_login and re.search(r"login:\s*$", text):
                    writer.write(b"admin\r\n")
                    sent_login, buf = True, b""
                elif not sent_cmd and re.search(r"[#$]\s*$", text):
                    # the markers are split by quotes, so the echoed command line never matches them
                    writer.write(f'echo "__GW3_""START__"; {cmd}; echo "__GW3_""DONE__"\r\n'.encode())
                    sent_cmd, buf = True, b""
                elif sent_cmd and DONE in text and START in text:
                    out = text[text.index(START) + len(START) : text.index(DONE)]
                    return out.replace("\r", "").strip("\n")
        finally:
            writer.close()
            try:
                await writer.wait_closed()
            except OSError:
                pass

    @staticmethod
    def _negotiate(data: bytes, writer: asyncio.StreamWriter) -> bytes:
        """Refuse every telnet option and strip the negotiation bytes."""
        out = bytearray()
        i = 0
        while i < len(data):
            if data[i] == 255 and i + 2 < len(data) and data[i + 1] in (251, 252, 253, 254):
                reply = 254 if data[i + 1] in (251, 252) else 252  # WILL/WONT -> DONT, DO/DONT -> WONT
                writer.write(bytes([255, reply, data[i + 2]]))
                i += 3
                continue
            out.append(data[i])
            i += 1
        return bytes(out)

    async def mac(self) -> str:
        """The gateway's MAC address, used as the config entry's unique id."""
        out = await self.run("grep ^mac= /data/miio/device.conf | cut -d= -f2")
        return out.strip().lower()

    async def bt_mode(self, action: str) -> bool:
        """Run gw3-btproxy.sh on|off|status; True when the proxy runs."""
        # a switch can wait up to 150 s for the lock (boot restore) and then needs up to 3 tries
        out = await self.run(f"sh {GW_DIR}/gw3-btproxy.sh {action}", timeout=240)
        lines = out.strip().splitlines()
        if any(line.startswith(("busy:", "cannot save")) for line in lines):
            raise GatewayError(f"gw3-btproxy.sh {action}: {lines[-1]}")
        return lines[-1:] == ["on"]

    async def boot_id(self) -> str:
        """The kernel's boot id plus the openmiio_agent pid; either change can reset the Zigbee chip."""
        out = await self.run(
            'echo $(cat /proc/sys/kernel/random/boot_id) '
            '$(ps | grep "openmiio_agent" | grep -v grep | awk "{print \\$1}")'
        )
        boot, _, pid = out.strip().partition(" ")
        if not boot:
            raise GatewayError("no boot id")
        return f"{boot}:{pid}"

    async def zigbee_tcp(self) -> bool:
        """openmiio_agent serves the Zigbee chip on TCP 8888 (Xiaomi Gateway 3 integration in ZHA mode)."""
        return ":8888 " in await self.run("netstat -tln | grep :8888")

    async def install(self) -> bool:
        """Copy the bundled files to the gateway when they differ. Returns True when something changed."""
        local = await asyncio.get_running_loop().run_in_executor(None, _local_md5s)
        # chmod first: a file that was replaced but not made executable (interrupted install) is repaired here
        files = " ".join(GW_FILES)
        out = await self.run(f"cd {GW_DIR} && chmod +x {files} 2>/dev/null; md5sum {files} 2>/dev/null")
        remote = {line.split()[1]: line.split()[0] for line in out.splitlines() if len(line.split()) == 2}
        stale = [name for name in GW_FILES if remote.get(name) != local[name]]
        if not stale:
            return False
        _LOGGER.info("Installing %s on gateway %s", ", ".join(stale), self.host)
        async with _FileServer(self.host) as url:
            for name in stale:
                try:
                    out = await self.run(
                        f"rm -f {GW_DIR}/{name}.new; wget -q -O {GW_DIR}/{name}.new {url}/{name} && "
                        f"chmod +x {GW_DIR}/{name}.new && md5sum {GW_DIR}/{name}.new",
                        timeout=120,
                    )
                except GatewayError:
                    out = ""
                if local[name] not in out:
                    try:
                        await self.run(f"rm -f {GW_DIR}/{name}.new")
                    except GatewayError:
                        pass
                    raise GatewayError(f"download of {name} failed: {out!r}")
        mv = " && ".join(f"mv {GW_DIR}/{n}.new {GW_DIR}/{n}" for n in stale)  # already executable
        if "gw3-btproxy" in stale and await self.bt_mode("status"):
            # The running proxy is replaced: stop it, move the files in, start the new one. One detached command
            # on the gateway, so it completes even if this telnet session or Home Assistant goes away meanwhile.
            # daemon_miio.sh runs Xiaomi's app in between; restore waits for that and starts the proxy (mode proxy).
            await self.run(
                f"(trap '' HUP; kill $(ps -ww | grep '{GW_DIR}/gw3-btproxy -tag' | grep -v grep | awk '{{print $1}}'); "
                f"sleep 2; {mv} && sh {GW_DIR}/gw3-btproxy.sh restore) </dev/null >/dev/null 2>&1 &"
            )
        else:
            await self.run(mv)
        return True


def _local_md5s() -> dict[str, str]:
    return {name: hashlib.md5((BIN_DIR / name).read_bytes()).hexdigest() for name in GW_FILES}


class _FileServer:
    """A short-lived HTTP server in Home Assistant that the gateway downloads the bundled files from."""

    def __init__(self, gateway_host: str) -> None:
        self._gateway_host = gateway_host
        self._runner: web.AppRunner | None = None

    async def __aenter__(self) -> str:
        app = web.Application()
        app.router.add_get("/{name}", self._serve)
        self._runner = web.AppRunner(app)
        await self._runner.setup()
        site = web.TCPSite(self._runner, "0.0.0.0", 0)
        await site.start()
        port = site._server.sockets[0].getsockname()[1]  # noqa: SLF001
        ip = await asyncio.get_running_loop().run_in_executor(None, _local_ip, self._gateway_host)
        return f"http://{ip}:{port}"

    async def __aexit__(self, *exc) -> None:
        if self._runner:
            await self._runner.cleanup()

    @staticmethod
    async def _serve(request: web.Request) -> web.StreamResponse:
        name = request.match_info["name"]
        if name not in GW_FILES:
            raise web.HTTPNotFound
        return web.FileResponse(BIN_DIR / name)


def _local_ip(remote: str) -> str:
    """The address of this host as the gateway sees it."""
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
        s.connect((remote, 23))
        return s.getsockname()[0]
