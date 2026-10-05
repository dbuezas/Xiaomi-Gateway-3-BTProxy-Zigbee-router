#!/usr/bin/env python3
"""Minimal Home Assistant websocket client (stdlib only), run inside an HA add-on with the supervisor token.

  ha_ws.py '<json message without id>' [...]   sends each message in turn and prints each result
"""
import base64
import json
import os
import socket
import struct
import sys

HOST, PATH = "supervisor", "/core/websocket"


def ws_connect():
    s = socket.create_connection((HOST, 80), timeout=600)
    key = base64.b64encode(os.urandom(16)).decode()
    s.sendall(
        f"GET {PATH} HTTP/1.1\r\nHost: {HOST}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n".encode()
    )
    resp = b""
    while b"\r\n\r\n" not in resp:
        resp += s.recv(1)
    if b" 101 " not in resp.split(b"\r\n")[0]:
        sys.exit(resp.decode(errors="replace"))
    return s


def send(s, obj):
    data = json.dumps(obj).encode()
    mask = os.urandom(4)
    head = bytes([0x81])
    n = len(data)
    head += bytes([0x80 | n]) if n < 126 else bytes([0x80 | 126]) + struct.pack(">H", n) if n < 65536 else bytes([0x80 | 127]) + struct.pack(">Q", n)
    s.sendall(head + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))


def recv_exact(s, n):
    buf = b""
    while len(buf) < n:
        chunk = s.recv(n - len(buf))
        if not chunk:
            raise EOFError
        buf += chunk
    return buf


def recv(s):
    payload = b""
    while True:
        b0, b1 = recv_exact(s, 2)
        n = b1 & 0x7F
        if n == 126:
            n = struct.unpack(">H", recv_exact(s, 2))[0]
        elif n == 127:
            n = struct.unpack(">Q", recv_exact(s, 8))[0]
        data = recv_exact(s, n)
        opcode = b0 & 0x0F
        if opcode == 0x9:  # ping: answer with a masked pong
            mask = os.urandom(4)
            s.sendall(bytes([0x8A, 0x80 | len(data)]) + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))
            continue
        if opcode in (0x8, 0xA):  # close, pong
            if opcode == 0x8:
                raise EOFError("closed by server")
            continue
        payload += data
        if b0 & 0x80:
            return json.loads(payload)


def main():
    token = open("/run/s6/container_environment/SUPERVISOR_TOKEN").read().strip()
    s = ws_connect()
    recv(s)  # auth_required
    send(s, {"type": "auth", "access_token": token})
    if recv(s).get("type") != "auth_ok":
        sys.exit("auth failed")
    for i, raw in enumerate(sys.argv[1:], 1):
        msg = json.loads(raw)
        msg["id"] = i
        send(s, msg)
        while True:
            r = recv(s)
            if r.get("id") == i and r.get("type") == "result":
                print(json.dumps(r))
                break


if __name__ == "__main__":
    main()
