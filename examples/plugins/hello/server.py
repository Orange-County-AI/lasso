#!/usr/bin/env python3
"""A minimal stdio MCP server for lasso's example plugin — standard library only.

MCP over stdio is newline-delimited JSON-RPC 2.0: one message per line on
stdin, one per line on stdout. Anything else a server prints (logs, tracebacks)
must go to stderr, or the client reads it as a malformed message. lasso runs
this inside a microsandbox microVM with no network, the plugin directory
mounted read-only at /plugin (the working directory).

It implements exactly what a client needs: initialize, the initialized
notification, ping, tools/list and tools/call, with one tool.
"""

import json
import os
import sys

SERVER_INFO = {"name": "hello", "version": "0.1.0"}

TOOLS = [
    {
        "name": "greet",
        "description": "Greet someone by name. Returns a friendly one-line greeting.",
        "inputSchema": {
            "type": "object",
            "properties": {"name": {"type": "string", "description": "Who to greet"}},
            "required": ["name"],
        },
    }
]


def log(*args):
    print("hello-plugin:", *args, file=sys.stderr, flush=True)


def send(msg):
    sys.stdout.write(json.dumps(msg, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def result(req_id, res):
    send({"jsonrpc": "2.0", "id": req_id, "result": res})


def error(req_id, code, message):
    send({"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}})


def greet(args):
    name = str(args.get("name") or "").strip() or "stranger"
    greeting = os.environ.get("GREETING", "Hello")
    return {
        "content": [{"type": "text", "text": f"{greeting}, {name}! (from a microVM: {os.uname().nodename})"}],
        "isError": False,
    }


def handle(msg):
    method = msg.get("method")
    req_id = msg.get("id")
    params = msg.get("params") or {}
    if req_id is None:
        # A notification (notifications/initialized, cancellations): no reply.
        return
    if method == "initialize":
        # Echo the client's protocol version: this server uses nothing that
        # differs between the revisions a client would ask for.
        result(req_id, {
            "protocolVersion": params.get("protocolVersion", "2025-06-18"),
            "capabilities": {"tools": {}},
            "serverInfo": SERVER_INFO,
        })
    elif method == "ping":
        result(req_id, {})
    elif method == "tools/list":
        result(req_id, {"tools": TOOLS})
    elif method == "tools/call":
        name = params.get("name")
        if name == "greet":
            result(req_id, greet(params.get("arguments") or {}))
        else:
            error(req_id, -32602, f"unknown tool {name!r}")
    else:
        error(req_id, -32601, f"method not found: {method}")


def main():
    log("started")
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError as e:
            error(None, -32700, f"parse error: {e}")
            continue
        try:
            handle(msg)
        except Exception as e:  # never let one bad request kill the server
            log("error handling", msg.get("method"), e)
            if msg.get("id") is not None:
                error(msg["id"], -32603, str(e))
    log("stdin closed, exiting")


if __name__ == "__main__":
    main()
