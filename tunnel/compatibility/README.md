# Tunnel agent/gateway compatibility harness

Black-box checks that frozen old tunnel binaries and current binaries keep
forwarding MCP traffic unchanged in every rollout order, and that optional
diagnostics never disturb the session or user traffic.

```bash
tunnel/compatibility/run.sh                       # full matrix + fault scenarios
tunnel/compatibility/run.sh -- -soak 10s -faults ""   # quick matrix only
tunnel/compatibility/run.sh --no-image -- -agents old-src,new-src
tunnel/compatibility/run.sh --old-ref <commit> --image <ref>
tunnel/compatibility/run.sh --main-ref <commit> --main-image <ref>
```

On macOS, a cold Docker start can take most of the default 30s admission
window; pass `-- -connect-timeout 60s` if an image agent is still starting.

The default run is sequential and takes about 40 minutes, mostly the 40s idle
soak in each of the 24 pairs. For a bounded run, build once with
`--build-only`, then start several `harness` processes against the same
`bin/` directory, each with its own `-out` directory: one per gateway kind
(`-gateways <kind> -soak 20s -faults= -transitions= -nearfull-streams 0`), one
long soak (`-agents new-src -gateways new-src -soak 90s` with the same skips),
and one for faults, near-full and rollbacks (`-agents= -gateways=`). Every pair
uses its own ports, target and container name, so the processes do not share
state; only CPU and Docker start-up are contended. A 20s soak still covers one
diagnostics poll interval but gives less idle time to catch sporadic probes.

`COMPAT_WORKDIR` (default `$TMPDIR/gram-tunnel-compat`) holds the frozen source
snapshots, binaries and `runs/<timestamp>/` with `provenance.json`,
`report.json` and per-pair process logs. The script exits non-zero if any
check fails.

## What gets built

| Kind                         | Source                                                                                                                                                                                        |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `old-src` agent and gateway  | `git archive <old-ref> go.mod go.sum tunnel` into a temp dir (default `ee14f2af29`, the 0.1.0 baseline), plus `testgateway/main.go` and `testgateway/gateway_old.go`. Never the working tree. |
| `old-image` agent            | Published `ghcr.io/speakeasy-api/gram-tunnel-agent:0.1.0`, run with Docker. The pulled digest, image ID and revision label are recorded.                                                      |
| `main-src` agent and gateway | The same export of `<main-ref>` (default `ecf54b81f4`, pre-feature main at tunnel 0.1.1).                                                                                                     |
| `main-image` agent           | Published `ghcr.io/speakeasy-api/gram-tunnel-agent:0.1.1`, recorded the same way.                                                                                                             |
| `new-src` agent and gateway  | Working tree. Gateway built with `-tags compatnew` (`gateway_new.go` enables `DiagnosticsEnabled`).                                                                                           |
| `faultagent`                 | Working tree. Misbehaving agent that advertises `diagnostics.v1`.                                                                                                                             |

`testgateway` runs the real `gateway` package with `StaticKeyStore` and an
in-memory route/connection snapshot store, exposing `/state` so the harness can
see what would be projected to Redis. It uses only APIs present at the
baseline. Put version-specific construction in the tagged files.

Provenance of the 0.1.0 image: index
`sha256:0948c23c16e16d9f7ada55e4ab99468c9dbf4e5fcd87328ee32767e78ea8a555`,
label revision `7274ada939268044a929fbf037a33abff8665021` (2026-07-07). Between
that revision and `ee14f2af29`, agent-relevant code differs only by header
constants in `tunnel/wire/key.go`, so the image and `old-src` should behave the
same. The 0.1.1 image (index
`sha256:115f5f5b2c8184cbd9c2b7caca0ab05dc454b4a5821bc401fbaa11f0919bb6cd`)
carries label revision `ebace10df2519c2282f905635e7bac839ca2d043`; `tunnel/`,
`go.mod` and `go.sum` are identical there and at `ecf54b81f4`. Both published versions still send agent version `0.1.0` in
the handshake, because 0.1.1 did not change the `wire.AgentVersion` constant.

## Path routing by version

`/` is forwarded to the path pinned in `TUNNEL_LOCAL_MCP_URL` by every version.
Other paths differ:

- 0.1.0 (`old-*`) joins them beneath the pinned path: `/echo` reaches
  `/mcp/echo`.
- 0.1.1 and later (`main-*`, `new-*`) keep the request's own path and query,
  after the pinned query, so tunneled OAuth back-channel calls reach the
  issuer's endpoints: `/oauth2/token` reaches `/oauth2/token`.

The fake target serves both layouts, and each check asserts the exact path
the agent's version must produce. `root_path_pinned` covers `/`, and
`oauth_nonroot_path` forwards a form-encoded token request and an RFC 8414
metadata request with an escaped issuer segment (`tenant%2Fa`), requiring the
method, path, escaped path, merged query and body to arrive unchanged.

## Matrix

Agents: `old-src`, `old-image`, `main-src`, `main-image`, `new-src`,
`new-src-nodiag` (`TUNNEL_DISABLE_DIAGNOSTICS=1`). Gateways: `old-src`,
`main-src`, `new-src`, `new-src-nodiag` (`TUNNEL_DIAGNOSTICS_ENABLED=0`). The
`old-*` and `main-*` builds both predate diagnostics. Every pair checks:

- Admission, route publication, and the legacy connection snapshot shape.
  Metadata must equal the configured `TUNNEL_METADATA` exactly.
- Method, version-specific path routing (above), query, request and response
  headers, and 2xx/4xx/5xx status and body passthrough. Internal `X-Gram-*` headers are stripped before
  the target.
- 8 MiB upload and download (compared by SHA-256), SSE streaming (the first
  event must arrive while the target withholds the second), and 64 concurrent
  forwards.
- Consumer requests to `/_tunnel/*` never reach the target, and
  unauthenticated status returns 404.
- Idle soak with one long-lived stream. Events arrive on time and the session
  stays single and stable. No HTTP reaches the target from diagnostics, and no
  TCP probes happen unless both sides negotiate diagnostics.
- One agent session for the whole run. No disconnects, and no WARN/ERROR or
  yamux `[ERR]` lines.
- The tunnel key, forward token, request payload, upstream credential, and
  target userinfo/query/fragment never appear in logs or snapshots. Old agents
  log the raw target URL at startup; this is reported as INFO because it
  predates this work.
- A gateway restart on the same port is followed by agent reconnect and
  forwarding.
- Diagnostics: a negotiated new/new pair must report `available` with a
  completed `reachable` sample and a sanitized `target_display`. Every other
  pair must carry no report.

Fault scenarios run against the diagnostics-enabled new gateway, each for 100s,
which is longer than yamux's 75s `StreamOpenTimeout`:

- `noaccept`: the agent never accepts yamux streams. Checks the open-timeout
  session kill.
- `slowstatus`: status is held for 60s.
- `notfound`: status returns 404. The session must switch to `unsupported`
  and receive exactly one poll.
- `malformed`: status returns invalid, oversized, or extra-field bodies that
  carry a sentinel.

Each scenario requires the session to survive, forwards to stay under 2s where
the agent serves them, diagnostics to never show `available`, and the sentinel
to stay out of snapshots and logs.

## Rollback transitions

- `gateway-rollback` and `gateway-rollback-main`: one running new agent stays
  configured for one public address while the gateway behind it is replaced
  new, then frozen `old-src` (or `main-src`), then new again. After each replacement the agent must be readmitted and forward.
  The old gateway must project the legacy shape, the new gateways must reach
  `available`, and the same agent process must connect exactly three times.
- `agent-rollback` and `agent-rollback-main`: one new gateway stays up while
  the new agent is stopped and replaced by the published 0.1.0 (or 0.1.1)
  image, or by `old-src` (or `main-src`) with `--no-image`. The new
  session must be released, and the old agent must be admitted, forward, report
  `0.1.0`, and show explicit `unsupported` with no report or target display.

## Limits

- Polls against old agents are not directly observable, because old agents
  answer 404 silently. The harness infers "no probes" from zero target
  connections and the absence of diagnostics fields.
- The harness has no negative control proving the `noaccept` scenario would
  catch a leaked stream. The mechanism is yamux's documented behavior: a local
  `Stream.Close` signals `establishCh`, which cancels the open timer.
- Linux runs the image with `--network host`. macOS uses
  `host.docker.internal`, which Docker Desktop forwards to host loopback.
