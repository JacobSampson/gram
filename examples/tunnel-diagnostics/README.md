# Tunnel diagnostics in the Gram dashboard

Five local HTTP targets and five real tunnel agents, viewed in Gram's MCP server
Overview → **Agents & target checks**. There is no separate demo dashboard and
no automatic MCP client, discovery, ping, or tool-call driver.

| Fixture   | Target behavior                                              | What Gram can honestly show                                                  |
| --------- | ------------------------------------------------------------ | ---------------------------------------------------------------------------- |
| `missing` | Hostname does not exist                                      | DNS failure after repeated probes                                            |
| `closed`  | Docker DNS works, nothing listens on port 8080               | DNS passes, TCP connection refused                                           |
| `silent`  | Listens; a caller's tool request waits without HTTP headers  | Network reachable; waiting-header count while normal traffic is pending      |
| `holding` | A caller's tool request receives HTTP 200/SSE, but no result | Network reachable; open-response count while the response stays open         |
| `healthy` | Responds normally to client requests                         | Network reachable; MCP successes only when normal traffic actually completes |

All five agents can be connected even when their target fails. HTTP targets show
TLS **Not applicable**. With no normal traffic, HTTP/MCP is **Not observed**.
An open SSE response is not itself a failure. Cases 3 and 4 cannot be distinguished
from a healthy idle server using DNS/TCP alone; the UI deliberately does not invent
that evidence. The fixture behavior is bounded to 90 seconds per caller request.
Native stdio is not supported by this tunnel and is outside this example.

## Start

Use a prepared Gram worktree with its local API, dashboard, tunnel gateway and
metrics consumer running. Enable the existing tunnel observability flags listed
in [the rollout plan](../../docs/tunnel-observability-plan.md). The setup command
accepts only a localhost `GRAM_SERVER_URL`; it creates private MCP servers named
`Tunnel lab missing`, `Tunnel lab closed`, `Tunnel lab silent`, `Tunnel lab holding`
and `Tunnel lab healthy`, with endpoints in the local `default` project.

From the repository root:

```sh
mise run wake
mise exec -- python3 examples/tunnel-diagnostics/demo.py setup
mise exec -- docker compose -p gram-tunnel-diagnostics-demo \
  --env-file examples/tunnel-diagnostics/.local/agents.env \
  -f examples/tunnel-diagnostics/compose.yaml up -d --build --remove-orphans
```

The setup uses `GRAM_API_KEY`, `GRAM_SERVER_URL`, and `NODE_EXTRA_CA_CERTS` from
the local mise environment. `DEMO_ORG_SLUG` defaults to the standard local org
slug; override it for a customized local seed. `DEMO_GATEWAY_URL` defaults to
`ws://host.docker.internal:${TUNNEL_GATEWAY_PUBLIC_PORT}/connect`. Docker targets
have no host-published ports. Credentials live only in ignored `.local/` files
with mode 0600, never in the Compose file.

Open **MCP Servers**, select each `Tunnel lab …` server, and open its Overview.
Allow up to two minutes for two failing probes and their display; reports and UI reads run
at 30-second intervals with jitter. First-failure and stale states stay unknown.
The fixtures are idle until a user connects their usual MCP client. No tool call
is required or issued to render the dashboard. If the client sends its own
requests, aggregate progress appears on the next report; completed MCP outcomes
appear in the existing activity graphs.

## Metrics and compatibility

Diagnostics carry only fixed aggregate counters and categorical transport
results, plus the permitted configured scheme/hostname/port/path. No request
records, arguments, results, headers, query strings, or payloads are collected.
Per-request recording does no I/O and uses constant-space gauges, independent
of traffic volume. Report frequency does not increase at 100+ requests/second.
Historical activity uses the existing bounded collector and minute request
buckets; gateway connection history is sampled every 15 seconds.

The `http_progress` field enriches optional `diagnostics.v1`. Older agents leave
progress unavailable; older gateways ignore the additional field. The agent's
forwarding protocol stays unchanged. See [compatibility validation](../../tunnel/compatibility/README.md).

## Stop and remove

```sh
mise exec -- docker compose -p gram-tunnel-diagnostics-demo \
  --env-file examples/tunnel-diagnostics/.local/agents.env \
  -f examples/tunnel-diagnostics/compose.yaml down --remove-orphans
mise exec -- python3 examples/tunnel-diagnostics/demo.py cleanup
```

Cleanup deletes only resources recorded by this example in `.local/state.json`.
The explicit Compose project name keeps this example separate from Gram's
worktree infrastructure.
