# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Inspect the selected Luvus UHP endpoint without changing its session."""
import json
import subprocess


def call(method):
    response = subprocess.run(
        ["luvus", "uhp", "proxy"],
        input=json.dumps({"id": "probe", "method": method, "params": {}}) + "\n",
        text=True,
        capture_output=True,
        check=True,
        timeout=5,
    )
    reply = json.loads(response.stdout)
    if "error" in reply:
        raise RuntimeError(reply["error"])
    return reply["result"]


capabilities = call("uhp.capabilities")
print("protocol:", json.dumps(capabilities["protocol"]))
print("methods:", ", ".join(capabilities["methods"]))
print("focused pane:", json.dumps(call("pane.current"), indent=2))
print("session:", json.dumps(call("session.snapshot"), indent=2))
