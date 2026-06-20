# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Probe the herdr newline-delimited JSON socket to confirm protocol + event types."""
import json
import os
import socket
import time

SOCK = os.path.expanduser("~/.config/herdr/herdr.sock")


def call(method, params=None, read_ms=400):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.connect(SOCK)
    req = {"id": "probe", "method": method, "params": params or {}}
    s.sendall((json.dumps(req) + "\n").encode())
    s.settimeout(read_ms / 1000)
    buf = b""
    try:
        while True:
            chunk = s.recv(65536)
            if not chunk:
                break
            buf += chunk
            if b"\n" in buf:
                break
    except socket.timeout:
        pass
    s.close()
    return buf.decode(errors="replace").strip()


# 1. confirm pane.list works + show focused pane's cwd
resp = call("pane.list")
try:
    panes = json.loads(resp)["result"]["panes"]
    focused = [p for p in panes if p.get("focused")]
    print("pane.list OK — focused pane:", json.dumps(focused, indent=2))
except Exception as e:
    print("pane.list parse error:", e, "raw:", resp[:200])

# 2. probe candidate event type names — valid ones ack, unknown ones error
for evt in ["pane.focused", "tab.focused", "workspace.focused",
            "pane.cwd_changed", "pane.updated", "focus.changed"]:
    r = call("events.subscribe", {"subscriptions": [{"type": evt}]}, read_ms=250)
    short = r[:160].replace("\n", " ")
    print(f"subscribe {evt!r:24} -> {short}")
