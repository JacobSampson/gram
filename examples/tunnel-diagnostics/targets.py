"""Synthetic MCP failure fixtures; no dependencies and no real data."""

import http.server
import json
import os
import select
import threading
import time

MODE = os.environ.get("MODE", "healthy")


def reply(request):
    method = request.get("method")
    if method == "initialize":
        result = {
            "protocolVersion": "2025-03-26",
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "tunnel-diagnostics-fixture", "version": "1"},
        }
    elif method == "tools/list":
        result = {
            "tools": [
                {
                    "name": "demo_ping",
                    "description": "Synthetic read-only ping",
                    "inputSchema": {"type": "object", "properties": {}},
                    "annotations": {"readOnlyHint": True, "idempotentHint": True},
                }
            ]
        }
    elif method == "tools/call":
        result = {"content": [{"type": "text", "text": "pong"}], "isError": False}
    elif method == "ping":
        result = {}
    else:
        return {
            "jsonrpc": "2.0",
            "id": request.get("id"),
            "error": {"code": -32601, "message": "Method not found"},
        }
    return {"jsonrpc": "2.0", "id": request.get("id"), "result": result}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def json(self, value, status=200):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.json({"process": "up", "note": "HTTP health does not prove MCP success"})

    def do_POST(self):
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 < size <= 65536:
                self.send_error(413)
                return
            request = json.loads(self.rfile.read(size))
            if "id" not in request:
                self.send_response(202)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            if request.get("method") == "tools/call" and MODE in ("silent", "holding"):
                if MODE == "holding":
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.send_header("Cache-Control", "no-cache")
                    self.send_header("Connection", "close")
                    self.end_headers()
                # Bounded fixture lifetime; stop immediately when the caller disconnects.
                until = time.monotonic() + 90
                while time.monotonic() < until:
                    if MODE == "holding":
                        self.wfile.write(b": still waiting for the synthetic tool\n\n")
                        self.wfile.flush()
                    readable, _, _ = select.select([self.connection], [], [], 1)
                    if readable and not self.connection.recv(1):
                        break
                self.close_connection = True
                return
            self.json(reply(request))
        except (BrokenPipeError, ConnectionResetError):
            pass
        except ValueError:
            self.json({"error": "fixture failure"}, 502)


if __name__ == "__main__":
    if MODE == "closed":
        # Container and Docker DNS exist, but no MCP socket is listening.
        threading.Event().wait()
    else:
        http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
