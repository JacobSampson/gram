> Evidence retention: historical `/tmp/` logs and `.playwright-cli/` captures
> referenced below are local-only, ignored artifacts. They are not durable PR
> artifacts and may disappear when this worktree is cleaned. The harness and
> commands are committed so the checks can be reproduced. Refreshed public GIFs
> on the PR are presentation evidence, not substitutes for the test reports.

# Tunnel observability implementation QA

Date: 2026-09-28. Base: `ecf54b81f4`; implementation commit: `2d625f0638`.
[PR #6874](https://github.com/speakeasy-api/gram/pull/6874) contains the implementation
and subsequent documentation updates. The current-main results below are
authoritative; earlier baseline reviews are superseded. No agent image was
published. Reproduce compatibility using [the harness](../tunnel/compatibility/README.md).

## Current-main integration

The implementation is rebased onto `ecf54b81f4`. The retired source detail page
is replaced by the unified MCP server Overview. The feature flag preserves the
legacy connections panel when disabled. Request collection is initialized in
all three serving modes: `gram start`, dedicated MCP, and private ingress.
The demo now includes an addressable endpoint for its private tunnel server.

Fresh verification after integration:

- 768 focused server tests passed across proxy, remote MCP, tunneled MCP,
  metrics storage, model views, demo seed, and command bootstrap.
- 160 command/bootstrap tests passed after wiring all serving modes.
- All tunnel packages passed; agent/gateway/collector/wire race tests passed.
- Demo seed safety passed 43 tests including the final endpoint addition.
- Dashboard type checking, Oxlint, and four status regressions passed; the
  fourth prevents cached successful history coloring an unavailable tile green.
- Server lint passed with only its existing deprecated-linter warning.

Current evidence is retained locally in `/tmp/tunnel-current-*.log` and
`.playwright-cli/pr-demos/`. Current compatibility, live recovery, and fresh
completion-review verdicts are recorded below.

Live current-main checks passed with a synthetic local target. The same agent
connection remained up through reachable → unknown (first failed probe) →
unreachable (`tcp_refused`) → reachable after target restart. Separate
Bearer-authenticated MCP probes returned 200 while healthy and 502 while the
target was stopped. `/tmp/tunnel-current-mcp-healthy.log` and
`/tmp/tunnel-current-mcp-failure.log` contain these statuses and API history
showing fresh request and connection buckets from the real Pub/Sub → streams →
ClickHouse pipeline. The background live-status recorder initially used the
management API header for MCP requests and consequently logged 401; those
requests are not evidence of successful forwarding. The separate authenticated
probes are the forwarding evidence.

Desktop and 390px mobile checks found no document overflow. PR captures contain
only the changed region and synthetic data; the local session prefix is redacted.
The final agent Docker build reports `0.2.0-dev` and remains unpublished.

`/tmp/tunnel-current-mcp-recovered.log` confirms both target-down calls were
stored as two errors (zero successes) in the matching minute, and the recovered
calls returned 200. Final server lint, dashboard typecheck, and Oxlint logs have
explicit `exit=0` markers. The cached-history UI regression was independently
rerun by Codex and passed.

Claude review follow-ups: restored Go import grouping, exposed server switches
through the existing CLI flag/EnvVars convention, and changed a wholly unchecked
target summary to “Not checked”. At that review, producers did not drain
in-memory aggregates on shutdown. The September 29 Cubic follow-up below adds a
bounded graceful drain; abrupt-exit and exhausted-budget loss remain possible,
including the absence of a prior-boot loss watermark. This
is diagnostic history, not a complete audit or billing ledger. Demo captions
identify seeded history and disclose the session-prefix redaction.

## Current-main compatibility and final review

Claude QA rebuilt frozen 0.1.0 (`ee14f2af29`) and pre-feature 0.1.1
(`ecf54b81f4`) fixtures and exercised both published images. All **24 pairs**
passed: six agents (both frozen sources, both published images, new, new opted
out) against four gateways (both frozen sources, new, new diagnostics disabled).
Each pair covers a 20-second soak, 8 MiB bodies, SSE, 64 concurrent forwards,
reconnect, privacy sentinels, root target pinning and non-root OAuth paths.
The harness explicitly preserves 0.1.0 path joining and 0.1.1+ path forwarding;
its earlier path-joining assumption was corrected without changing production.

Additional results: **9/9** diagnostic-fault, near-full and rollback scenarios;
a **90-second** new/new soak; and three clean published-0.1.1/baseline rechecks.
Rollback replaces gateways new→0.1.0→new and new→0.1.1→new, and replaces the
new agent with each published image. Four final pairs were rebuilt and rerun
after import-only formatting changes; all passed. Final tunnel race tests passed.
Authoritative local evidence is `.playwright-cli/tunnel-qa/current/`: `SUMMARY.md`,
`bounded/*/report.json`, `bounded/provenance-tested.json`, and
`final-smoke/20260928T145644Z/{report,provenance}.json`. The final tunnel diff hash
is `4500f4390ab5` (prefix), independently matched by the Claude reviewer.

Invalid and interrupted attempts remain alongside the valid reports. A
mis-quoted, overloaded launch recorded 61 idle TCP connections for the published
0.1.1 agent; this was not reproduced in four clean runs and its cause is
unproven. A harness client timeout also truncated the initial 90-second soak;
the corrected client deadline passed. These attempts are not counted as passes.
The 24-pair matrix covers about one diagnostic polling interval per pair;
only new/new received the longer soak. Published 0.1.1 still reports wire agent
version 0.1.0, a pre-existing version-constant issue; new builds inject the version.

Live current-main tool validation (`/tmp/tunnel-current-toolcalls.log`) returned
HTTP 200 for both a successful call and an MCP `isError` result. The corresponding
history minute contains two tool calls and one list call, two successes and one
error. This proves semantic errors reach aggregate history through the real
local producer/queue/consumer/database/API pipeline. After disconnect, observed
zero-connection buckets retain coverage instead of becoming gaps immediately.

Fresh independent Claude and Codex completion reviewers used standalone
requirements and current artifacts. Claude's final verdict: **implementation and verification
complete, ready for PR; no remaining code, privacy, compatibility or guidance
defect**. It independently ran 743 server tests, all tunnel tests and vet,
four UI tests, dashboard type checking, Oxlint and formatting. Codex's final implementation verdict: **PASS for the scoped, gated local delivery;
no material implementation finding remains**. It independently parsed all 37
bounded cases plus four final rebuilt pairs, matched the final source hash and
all six binary hashes, and reran 160 bootstrap tests and four UI regressions.
The cached-history color regression and Platform MCP assessment were resolved
and re-reviewed. Both reviewers subsequently approved the published PR and demos;
CI and production release gates are separate from their completion verdicts.

PR screenshots show synthetic seeded history with a real local agent/target;
the local session prefix is redacted. Only the clean overview and target-failure
crops are published. Mobile layout was checked but its dev-overlay capture is
not a public demo. Production image publication, IAM, customer-network canaries
and fleet-scale capacity remain release gates; PR publication does not claim
those completed.

Delivery: [PR #6874](https://github.com/speakeasy-api/gram/pull/6874) contains the
implementation and both inspected screenshot comments. Downloaded attachment
hashes match the reviewed local PNGs. CI initially required the `mig:` title
prefix because this change includes ClickHouse migrations; the title was
corrected. CI is separate from the local validation above and was still running
at publication. The worktree is retained for follow-up; no image was published.

## Passive HTTP progress follow-up

The `0.2.0-dev` local agent enriches `diagnostics.v1` with optional aggregate
waiting-header/open-response gauges. Retained state is constant-size; forwarding
only updates counters, and response bodies are never parsed or recorded.
Diagnostics and UI polling now use 30-second intervals. Existing gateway history
sampling remains 15 seconds and MCP request history uses minute buckets.

Validation of this follow-up:

- Race tests passed for agent, wire, gateway and route, including 10,000 concurrent
  aggregate updates, body EOF/close idempotence, upgrade writer preservation,
  forwarding integrity, cancellation and payload-sentinel exclusion.
- 37 model-view tests passed; 24 focused dashboard tests passed. Dashboard type
  checking and pinned server lint passed (only a linter deprecation warning).
- Local parallel microbenchmark: approximately 635 ns per observed request versus
  69 ns baseline, with one additional body-wrapper allocation. This is a recorder
  microbenchmark, not a fleet throughput or latency claim.
- The five Compose fixtures were rebuilt and viewed in the actual Gram Overview.
  DNS absence and connection refusal are distinct; the other three are network
  reachable. All agents report zero HTTP requests until normal traffic arrives.
  Fresh fixture resources replaced the earlier development exerciser's history.
- An existing global assistant eagerly initialized MCP clients when mounted even
  while collapsed. Its client discovery is now deferred until assistant/chat use,
  so a passive Overview does not initiate MCP discovery. Explicit assistant use
  still performs normal discovery, and existing Tool I/O logging is unchanged.
- The compatibility run exercised 24 old/new/disabled pairings, four rollback
  transitions and near-full pressure. 27 of 29 scenarios passed initially. A
  published 0.1.0/frozen-old pair saw 63 idle TCP accepts in one timestamped burst,
  with zero HTTP; its isolated rerun passed with zero idle accepts. A published
  0.1.1/new-gateway container produced no startup log and missed the 30-second
  admission deadline; its isolated rerun passed admission, forwarding, idle checks
  and reconnect. Both anomalous original results are retained locally; their exact
  environmental causes were not established. No compatibility failure reproduced
  in those targeted reruns.
- After adapting the harness timing to the slower cadence, an independent
  70-second pressure run passed: 232 streams delivered all 36 SSE events, five
  additional forwards succeeded, diagnostics paused and resumed without teardown.
  The harness client deadline now includes the requested pressure duration.

The silent and holding cases intentionally cannot demonstrate protocol failure
while idle. Non-zero gauges are covered by transport tests without issuing a
synthetic MCP call to the running fixtures. Open SSE is not itself an error.
Short spikes between reports can be missed; metrics remain best-effort observations.

Fresh completion reviews used `claude-danger` (Claude) and `codex-danger` (Codex)
in separate visible Herdr panels with no inherited working conversation. Both
reviewers inspected the final source, fixtures, tests, browser captures and
compatibility evidence and passed the scoped local delivery with the limitations
above. The final browser check returned zero MCP requests over 35 seconds after
reloading Overview; the five-page navigation check also passed with zero MCP
requests. Owner-restricted UI captures are local review artifacts, not a separate
product UI or a production deployment.

## September 29 Cubic review follow-up

The follow-up fixes cover bounded publisher shutdown, accurate MCP outcomes,
independent history enablement, strict required-field presence with additive
unknown-field handling, source churn/retention, UI state handling, and safer,
stronger compatibility checks. Main was merged at `679c03b6cf` before final checks.
The full comment disposition is in [the review ledger](tunnel-observability-review.md).

Fresh local verification:

| Check                                                             | Result                                  |
| ----------------------------------------------------------------- | --------------------------------------- |
| Focused backend suite (proxy, manager, model views, storage, API) | 610 passed                              |
| Dashboard feature, query-failure and assistant tests              | 43 passed                               |
| Dashboard type check and pinned server lint                       | Passed; linter deprecation warning only |
| Agent, wire, collector, gateway and route race tests              | Passed                                  |
| Publisher final-flush/stop ordering race test                     | Passed                                  |
| Demo-seed safety checks                                           | 43 passed                               |
| Frozen/released/new/opted-out compatibility matrix                | All 24 pairs passed                     |
| Unacknowledged, unread, slow and malformed diagnostic peers       | All four scenarios passed               |
| Agent/gateway rollback transitions                                | All four passed                         |
| Near-full yamux pressure                                          | 232 held streams, 70-second hold passed |

Final matrix reports are local-only under
`/tmp/gram-tunnel-cubic/run.BkaYKiR3/reviewed-{old-src,main-src,new-src,new-src-nodiag}/report.json`;
faults and pressure/rollback reports are under `faults/` and
`transitions-nearfull/` in the same root. Build/image provenance is retained in
that run directory. Logs are `/tmp/tunnel-cubic-*.log`. These paths are not durable
CI artifacts; use the compatibility README to reproduce the checks.

Earlier attempts exposed a harness assertion incorrectly requiring a new control
response from frozen gateways; it now requires legacy 200 or new 404 explicitly.
Two earlier post-burst idle checks observed 63 target TCP accepts with zero HTTP.
An isolated rerun passed. The first final matrix measures idle before burst traffic and
all 24 pairs pass. A review follow-up adds a second, at least 35-second HTTP-idle
window after traffic. All 24 expanded pairs pass (664 checks), including zero
target HTTP requests during every post-traffic window. The harness source is
committed at `5dc842c647`; the reviewed harness binary SHA-256 is
`c7281b61c74f3d27751a095c8a24fb59f9eadf36690c1e40cff354cd354514a7`. Late speculative dials are a possible explanation, not a proven
cause; original reports remain alongside the final runs.

The refreshed history capture exercises 1 hour, 24 hours, 7 days, the data table
and no-agent state against synthetic seed history. The target capture cuts between
five independent, idle Docker Compose fixtures. It shows DNS failure, connection
refusal, and three network-reachable targets; HTTP/MCP remains Not observed.
Both capture sessions observed zero browser requests to the MCP endpoint; the
agent cards also show no observed HTTP traffic. No
synthetic discovery or tool calls were used. Captures crop local account details,
hide the unrelated development overlay, and redact session prefixes only for
publication. Raw frames and exported GIFs remain under the ignored
`.playwright-cli/pr-demos/6874/` directory.

Independent review found and fixed an additional UI failure mode: history and
live queries now opt out of the global error boundary so one failed request does
not hide the other section. Real QueryClient regressions both fail with the
fix removed and pass with it restored. The graceful-drain documentation and GIF
crops were corrected, and post-traffic idle coverage was restored.

Published refreshed demos replace the existing PR comments:
[history](https://github.com/speakeasy-api/gram/pull/6874#issuecomment-5872646970)
and [target checks](https://github.com/speakeasy-api/gram/pull/6874#issuecomment-5872654311).
Both 12-second GIF URLs returned HTTP 200 with image/gif and bytes matching the
locally inspected exports.

Fresh independent reviewers ran in visible Herdr panels using `claude-danger`
and `codex-danger`, with requirements and artifact paths but no inherited working
conversation. Both passed the corrected implementation and demos on re-review.
Claude additionally ran 211 dashboard-tab tests and checked the published GIF
hashes; Codex independently reran 610 backend tests, tunnel race tests, the new
query regressions and dashboard type checking, and verified all 24 expanded
compatibility reports. Review findings and their fixes are recorded above.

Remaining limits: metrics are bounded best-effort observations, previous-boot loss
cannot be quantified, and idle transport reachability does not prove MCP success.
Raw compatibility evidence is local-only, and earlier TCP-only anomalies retain
unproven causes. Production IAM, image publication, network canaries and fleet
capacity remain rollout work; this follow-up does not claim those gates passed.
