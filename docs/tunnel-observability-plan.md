# Tunnel observability

Implementation on `feat/tunnel-observability`, based on `ee14f2af29`.
The agent development version is `0.2.0-dev`; the minor changeset releases `0.2.0`.
The implementation is being prepared for review; the new agent image is not yet published.

## Operator experience

The tunnel source Overview separates three questions:

1. **Tunnel connections:** agents currently present in Gram's route store. A route-store failure says unavailable, not disconnected.
2. **Target transport:** DNS, TCP and TLS evidence collected from each agent's network. A connected agent can have an unreachable target. Missing, stale, unsupported and disabled diagnostics do not become target failures.
3. **MCP activity:** actual tool calls, tools/list, terminal outcomes, latency histograms, connections opened, sampled connections/consumer sessions/substreams, linked servers and bounded client families. Charts cover 1 hour, 24 hours or 7 days, with a keyboard-readable data table. The linked-server tile also reports how many servers had requests in the selected range.

The target display includes scheme, hostname, port and path, as authorized. It excludes userinfo, query and fragment. It is current source configuration, not a historical metric dimension. Refreshing the page reads cached diagnostics; it does not trigger probes.

Existing Tool logs settings and payload capture are unchanged. This feature neither adds payload logging nor changes that separate product surface.

## Additive protocol

The existing WebSocket/yamux hello and forwarding protocol are unchanged. A capable agent advertises `diagnostics.v1`, a cryptographically random per-session control token and a sanitized target display in optional handshake headers. An old gateway ignores these. An old agent omits them, and a new gateway reports unsupported without polling it.

A new gateway with diagnostics enabled makes authenticated `GET /_tunnel/status` requests over a dedicated yamux substream. Consumer access to reserved control paths stays rejected. Control headers are removed before forwarding; wrong tokens return 404. A 404 stops diagnostic polling for that session. There is no target address or executable action in a control request.

Reports contain a version, relative sample ages, monotonic sequence, bounded DNS/TCP/TLS results, consecutive failures, passive HTTP status and process request/transport-error counters. Only fixed categories and scalar values survive typed decoding. Report bodies are capped at 8 KiB, response headers/body reading is bounded, and malformed or unsupported fields cannot block admission or routing.

Default polling is 30 seconds with jitter, freshness 90 seconds, timeout 5 seconds. One opener per session and 32 per gateway are allowed. Polling skips sessions with at least 224 yamux streams and backs off failed polls up to four minutes. Slot contention retains old evidence until it naturally becomes stale. A timed-out opener keeps its concurrency slot until it returns; closing the diagnostic stream cancels yamux's open timer, never the forwarding session.

The agent probes only its startup-pinned target while recently polled. It stops active probes after a 60-second polling lease expires. Active checks stop at DNS/TCP/TLS; they send no HTTP, MCP initialization, tool list or tool calls. Proxy-controlled transport is explicitly unobservable rather than probed directly. Passive forwarding observes HTTP status and typed transport errors without reading bodies. An HTTP 401 is a response, not proof that the transport is dead. The UI requires two consecutive failed probes before its aggregate target state becomes unreachable; it still shows the first failing step immediately. A successful probe recovers it.

## Payload-free history

The Gram proxy records one semantic request attempt and at most one terminal outcome. A retry to another gateway remains one logical request. JSON-RPC errors, tool `isError`, policy rejection, cancellation and incomplete streams are separate outcomes. Attempts belong to their observation minute; terminal outcomes and durations belong to their completion minute. These are different denominators, so the UI does not divide same-minute completions by attempts.

The typed recorder accepts only Gram-owned source/server IDs, an allowlisted MCP method, a fixed client family, outcome and duration. It never receives bodies, tool names, raw user agents, request IDs, credentials or raw error strings. Raw user agents are classified before the callback and validated again at ingestion. Agent reports cannot choose tenant identity.

The gateway independently samples connection gauges at aligned 15-second boundaries and records admissions. It continues observing known sources for five minutes after their last connected sample, reporting explicit zero while its collector is alive. After that bounded lease, gaps are unknown. Gateway aggregates carry a conservative loss watermark separately from MCP outcomes. Gram request producers emit minute coverage heartbeats for sources they have observed, including subsequent idle periods. A heartbeat proves only that producer's coverage. Collector loss is a conservative partial-coverage watermark until that producer restarts. Missing samples remain null; no history is backfilled from Tool logs.

Accumulators hold at most 100,000 series, with reserved coverage capacity and at most 10,000 tracked sources. Recording does no I/O. A flush uses 16 workers, two-second publish deadlines and a 12-second overall deadline. Failed/unsent cumulative snapshots retry for at most ten minutes while the producer is running. Shutdown does not drain the in-memory collector: graceful and abrupt exits can lose the latest roughly 15 seconds plus any pending retries. A new boot cannot quantify that loss, so its loss watermark does not cover the previous process. This bounded, best-effort tradeoff is accepted for diagnostic history; these counts must not be used as a complete audit, billing, or SLA record. Pub/Sub buffers are bounded to 1,000 messages / 16 MiB; errors never block forwarding. Publisher initialization has a five-second deadline and falls back to disabled collection.

A declared Pub/Sub topic feeds a `gram streams` batch consumer (10,000 messages / 10 MiB / five seconds). There are no Temporal actions. The consumer validates fixed dimensions and numeric bounds, resolves source-to-project ownership in one authoritative SQL lookup, drops unknown/deleted sources, and acknowledges only after a successful durable ClickHouse batch insert (`wait_for_async_insert=1`).

ClickHouse retains seven days in daily partitions. Project/source/time prefix the ordering key. `ReplacingMergeTree(revision)` plus query-time `argMax` deduplicates each producer/source/dimension/bucket before cross-producer summation; retries and out-of-order revisions do not inflate totals. Histogram bins merge before percentile calculation. The API caps reads at 200,000 rows and five seconds and returns a distinct `too_large` state with a shorter-range action if exceeded. This is diagnostic evidence, not a billing ledger or availability SLA.

Connection history averages timestamp-aligned sums of observed gateway samples. Missing owners cannot be inferred as zero. A producer restart gets a new boot ID; two gateway boots in one 15-second interval can both contribute to that interval. Fleet rollout must evaluate this bounded sampling ambiguity and collector/query capacity. No full-fleet SLO is claimed by local tests.

## Access and implementation map

- `tunnel/wire/diagnostics.go`: optional wire contract and sanitization.
- `tunnel/agent/diagnostics.go`: transport checks and passive observation.
- `tunnel/gateway/diagnostics.go`: isolated polling and bounded failure handling.
- `tunnel/metrics/`, `tunnel/metricspub/`: bounded aggregates and typed publication.
- `infra/proto/gram/tunnel/v1/`: topic and batch-consumer declaration.
- `server/internal/remotemcp/proxy/observation.go`: payload-free semantic observer shared by tunnel-backed proxy paths.
- `server/internal/tunnelmetrics/`: authoritative ingest and deduplicated reads.
- `server/design/tunneledmcp/design.go`: additive live fields and `getServerMetrics` management method; generated Goa/OpenAPI/SDK output accompanies it.
- `client/dashboard/src/pages/mcp/x/tabs/TunnelObservability.tsx`: Overview UI.

Both history and connection reads require the existing `mcp:read` policy and project-scoped source lookup. Client-supplied source IDs cannot cross project boundaries. Readers must key on diagnostic state, not report presence: a failed poll retains the previous report and its original receipt time. Current diagnostic observations use gateway receipt time plus validated relative age, not the agent's wall clock or legacy route heartbeat timestamp.

## Platform MCP assessment

Outcome: an operator with project-level `mcp:read` diagnoses a particular tunnel
source, distinguishes connected agents from reachable targets, and reads bounded
activity across every MCP server sharing that source. Host/port/path are permitted
on this management surface; request payloads and credentials are not.

Compared existing tools and shipped workflows:

- `get_mcp_diagnostics` (`platformmcp/tool_diagnostics.go`,
  `DiagnosticsService.GetMCPDiagnostics`) admits external members and managed
  assistants using `project:read`, explicitly without a separate `mcp:read`
  grant. It returns existing MCP readiness and telemetry outcomes for one MCP.
- `get_mcp_connection_settings` (`platformmcp/tool_connection_settings.go`)
  admits external administrators with explicit project targeting and `mcp:read`.
  Its contract explicitly excludes upstream URLs and describes stored endpoint
  and ingress configuration, not agent-side transport observations.
- Registration in `platformmcp/tools.go` preserves those audience contracts.
  Shipped connection-settings and migration workflows do not promise tunnel
  agent diagnostics; the migration workflow explicitly excludes tunnelled MCPs.

Decision: **omit new Platform MCP fields/tools in this gated rollout**. Passing
source-wide observations and internal target paths through delegated diagnostics
would widen both resource scope and disclosure beyond its current authorization
contract. Adding them to connection settings would violate its no-upstream-URL
contract and conflate configuration with live checks. Neither external nor
managed-assistant audiences inherit this new disclosure implicitly. Existing
MCP tools keep their current bounded outcomes; this is a deliberate scope limit,
not a claim that their existing server-side checks prove tunnel-target health.
A future agent-facing outcome should reuse the diagnostics tool only after
explicit source-level authorization, audience, and target-address disclosure
rules are defined and tested.

Evidence: `TestDelegatedDiagnosticsToolsRequireProjectReadDiscovery` and
`TestGetMCPDiagnosticsOutput_ProjectsOnlyAllowlistedFields` fix the existing
contract; tunnel management authorization and cross-project denial are covered
in `server/internal/tunneledmcp/metrics_test.go`. No Platform MCP manifest,
permission, or distributed skill is changed by this release.

## Rollout, opt-out and rollback

Deploy schema and generated Pub/Sub topology/IAM first, then the consumer, API and gateway. Enable producers only after verifying ingestion. The independent switches default off:

| Switch                                   | Process                | Enables                           |
| ---------------------------------------- | ---------------------- | --------------------------------- |
| `GRAM_TUNNEL_METRICS_CONSUMER_ENABLED=1` | `gram streams`         | Metrics batch consumer            |
| `GRAM_TUNNEL_METRICS_ENABLED=1`          | Gram server            | Request recording and history API |
| `TUNNEL_METRICS_ENABLED=1`               | Tunnel gateway         | Connection history publication    |
| `TUNNEL_DIAGNOSTICS_ENABLED=1`           | Tunnel gateway         | Polling capable agents            |
| `gram-tunnel-observability`              | Dashboard feature flag | New Overview                      |

The server flags are `--tunnel-metrics-enabled` and `--tunnel-metrics-consumer-enabled`, backed by the environment switches above. Both default off.

Gateway publication also requires `GRAM_GCP_PROJECT_ID` and service IAM to publish the declared metrics topic. Local development uses `PUBSUB_EMULATOR_HOST`.

Upgrade the customer agent using the usual image release. Existing required environment variables are unchanged:

```sh
docker run --rm \
  -e TUNNEL_GATEWAY_URL -e TUNNEL_KEY -e TUNNEL_LOCAL_MCP_URL \
  -e TUNNEL_SERVICE_VERSION \
  ghcr.io/speakeasy-api/gram-tunnel-agent:0.2.0
```

`0.2.0` is the planned release tag, not a published artifact from this branch. `--version` prints the embedded agent version. Local verification built `gram-tunnel-agent:observability` with `VERSION=0.2.0-dev`.

Set `TUNNEL_DISABLE_DIAGNOSTICS=1` on an agent to suppress its new capability, target display, active probes and passive diagnostic counters. Disable the gateway diagnostic flag to stop polling without changing routes. Disable either metrics producer independently to stop history for that producer; keep the consumer running to drain queued snapshots. Roll back agents or gateways in either order; older fields and forwarding remain valid. Retain schema/topology until data and queued messages expire rather than dropping them during rollback.

## Validation and remaining release gates

The reproducible black-box harness is [tunnel/compatibility/README.md](../tunnel/compatibility/README.md). It freezes baseline source, also runs the actual published 0.1.0 image, and retains image/source provenance. Its matrix covers old/new/disabled agents and gateways, large bodies, SSE, concurrency, gateway restart, reserved paths, sanitization, stalled/malformed diagnostics and near-full yamux sessions. The implementation QA report records final results and reviewer verdicts separately.

Local tests cover transport-only probes, untrusted TLS, HTTP 401 interpretation, wire sanitization, cumulative revisions, bounded 10,000-source publication, request outcomes/retries/rejections, real ClickHouse duplicate/out-of-order handling, authorization, null gaps and coverage, dashboard type checking and demo-seed isolation. A synthetic demo source has 24-hour history with a recent gap and one linked server; it deliberately has no live agent.

Before a broad production rollout: inventory deployed versions; verify publisher IAM and ingress header redaction; canary fast and slow fleets at expected scale; measure forwarding overhead, report freshness, memory, insert volume and query p95; exercise private/public/session-pinned/meta-MCP deployments in their real routing configuration. DNS/proxy/TLS behavior is tested locally but cannot guarantee every customer's network. Gateway process health is not target health, and a disconnected agent alone cannot identify a gateway outage.

The unified MCP server Overview shows the optional source-wide charts and diagnostics when flagged on, and retains the existing connections panel when off. The existing server-specific usage section stays available below. Publication was tested against the emulator; production credential refresh with the gateway publisher initialization context is an explicit IAM canary check.

## Passive HTTP progress and local examples

The optional `http_progress` enrichment reports only aggregate counts of requests
awaiting response headers and response bodies still open. Counters use constant
space and no I/O in the forwarding path; body reads are passed through without
inspection or per-chunk recording. EOF/error/close releases each gauge once.
Long-lived SSE is not classified as failed. Older agents omit the field and the
UI says unavailable rather than treating missing data as zero.

The dashboard samples cached diagnostics every 30 seconds. Probes issue DNS/TCP/TLS
only: no MCP discovery, ping, or tool calls. No normal traffic means HTTP/MCP
not observed. An HTTP 200 alone never proves MCP completion.
The [five-case Docker Compose example](../examples/tunnel-diagnostics/README.md)
creates real local agents and targets for the actual Gram Overview. Native stdio
is not supported; plaintext HTTP targets report TLS as not applicable.

Viewing a passive dashboard page also leaves the collapsed Project Assistant's
client MCP configuration empty. Discovery becomes available when the user opens
the assistant, enters chat, uses a page-owned chat surface, or queues a prompt.
This fixes pre-existing eager initialization from the global assistant; the
assistant's user-triggered discovery remains normal traffic, separate from health
checks. Existing Tool I/O logging remains the place for request/response data.
