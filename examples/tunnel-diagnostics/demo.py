"""Local demo control plane. Credentials stay in a gitignored, mode-0600 file."""

import argparse
import json
import os
import ssl
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent
LOCAL = ROOT / ".local"
CASES = [
    ("missing", "01 · Host does not exist"),
    ("closed", "02 · DNS, but no server"),
    ("silent", "03 · Up, but no response"),
    ("holding", "04 · Connection held open"),
    ("healthy", "05 · Healthy target"),
]


def context():
    return ssl.create_default_context(cafile=os.environ.get("NODE_EXTRA_CA_CERTS"))


def rpc(path, data=None, method=None):
    req = urllib.request.Request(
        os.environ["GRAM_SERVER_URL"] + "/rpc/" + path,
        data=None if data is None else json.dumps(data).encode(),
        headers={
            "Gram-Key": os.environ["GRAM_API_KEY"],
            "Gram-Project": "default",
            "Content-Type": "application/json",
        },
        method=method,
    )
    with urllib.request.urlopen(req, context=context(), timeout=10) as response:
        body = response.read()
        return json.loads(body) if body else None


def private_write(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as file:
        file.write(value)


def setup():
    # A demo must never silently provision a production project.
    host = urllib.parse.urlsplit(os.environ["GRAM_SERVER_URL"]).hostname
    if host not in ("localhost", "127.0.0.1"):
        raise SystemExit("This fixture only provisions a localhost Gram API")
    LOCAL.mkdir(exist_ok=True, mode=0o700)
    state_file = LOCAL / "state.json"
    state = json.loads(state_file.read_text()) if state_file.exists() else {}
    for key, title in CASES:
        if key in state:
            continue
        source = rpc("tunneledMcp.createServer", {"name": "Tunnel lab " + key})
        state[key] = {"source": source["server"]["id"], "key": source["tunnel_key"]}
        private_write(state_file, json.dumps(state, indent=2))
    for key, title in CASES:
        if "server" not in state[key]:
            server = rpc(
                "mcpServers.create",
                {
                    "name": "Tunnel lab " + key,
                    "visibility": "private",
                    "tunneled_mcp_server_id": state[key]["source"],
                },
            )
            state[key]["server"] = server["id"]
            state[key]["slug"] = server["slug"]
            private_write(state_file, json.dumps(state, indent=2))
        if "endpoint" not in state[key]:
            endpoint_slug = (
                os.environ.get("DEMO_ORG_SLUG", "speakeasy") + "-" + state[key]["slug"]
            )
            endpoint = rpc(
                "mcpEndpoints.create",
                {"mcp_server_id": state[key]["server"], "slug": endpoint_slug},
            )
            state[key]["endpoint"] = endpoint["id"]
            state[key]["endpoint_slug"] = endpoint_slug
            private_write(state_file, json.dumps(state, indent=2))
        print("Ready:", title)
    gateway = os.environ.get(
        "DEMO_GATEWAY_URL",
        "ws://host.docker.internal:"
        + os.environ.get("TUNNEL_GATEWAY_PUBLIC_PORT", "46932")
        + "/connect",
    )
    private_write(
        LOCAL / "agents.env",
        "DEMO_GATEWAY_URL="
        + gateway
        + "\n"
        + "\n".join("KEY_" + k.upper() + "=" + v["key"] for k, v in state.items())
        + "\n",
    )


def cleanup():
    state = json.loads((LOCAL / "state.json").read_text())
    for key, values in state.items():
        for field, service in [
            ("endpoint", "mcpEndpoints.delete"),
            ("server", "mcpServers.delete"),
            ("source", "tunneledMcp.deleteServer"),
        ]:
            if field not in values:
                continue
            try:
                rpc(service + "?id=" + values[field], method="DELETE")
            except urllib.error.HTTPError as error:
                if error.code != 404:
                    raise
        print("Removed local demo resources:", key)
    for filename in ("state.json", "agents.env"):
        (LOCAL / filename).unlink(missing_ok=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["setup", "cleanup"])
    args = parser.parse_args()
    if urllib.parse.urlsplit(os.environ.get("GRAM_SERVER_URL", "")).hostname not in (
        "localhost",
        "127.0.0.1",
    ):
        raise SystemExit("This fixture only connects to a localhost Gram API")
    if args.action == "setup":
        setup()
    else:
        cleanup()
