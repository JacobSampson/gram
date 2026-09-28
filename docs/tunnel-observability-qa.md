# Tunnel observability implementation QA

Date: 2026-09-28. Worktree: `gram.feat-tunnel-observability`, branch `feat/tunnel-observability`, base `ee14f2af29e033908225f2f853d3a67edb1b71fc`. This records the original implementation validation before integration with current main. No agent image was published by this work.

## Historical baseline compatibility

Claude QA used a fresh `claude-danger` session in a visible, unfocused Herdr panel. It could edit only `tunnel/compatibility/`; production fixes were made by the implementing agent.

The full run `20260928T134626Z` passed all 16 scenarios:

| Agent                             | Old gateway | New gateway | New gateway, diagnostics off |
| --------------------------------- | ----------- | ----------- | ---------------------------- |
| Frozen baseline 0.1.0 source      | PASS        | PASS        | PASS                         |
| Published 0.1.0 image             | PASS        | PASS        | PASS                         |
| New 0.2.0-dev source              | PASS        | PASS        | PASS                         |
| New source, diagnostics opted out | PASS        | PASS        | PASS                         |

The other four passing scenarios were never-accepted diagnostic streams, slow status responses, malformed/oversized reports, and 232 held streams near the yamux limit. Fault scenarios ran 100 seconds, beyond yamux's 75-second open timeout. The near-full test held streams for 45 seconds and verified polling resumed after release. A negative control at 200 streams correctly failed its “polling paused” assertion.

The matrix verifies 8 MiB upload/download hashes, status/header/body forwarding, early SSE delivery, 64 parallel forwards, a long-lived stream, gateway-restart reconnect, reserved-control-path isolation, capability/opt-out behavior, sanitized target configuration, and secret sentinels absent from new diagnostic outputs. Old agents' pre-existing raw-target startup logging is recorded separately; this feature does not claim historical logs are payload-free.

Published image provenance:

- Image: `ghcr.io/speakeasy-api/gram-tunnel-agent:0.1.0`.
- Index digest: `sha256:0948c23c16e16d9f7ada55e4ab99468c9dbf4e5fcd87328ee32767e78ea8a555`.
- Source label: `7274ada939268044a929fbf037a33abff8665021`.
- Baseline fixtures are exported with `git archive`, never rebuilt from the modified worktree.

The full report and binary provenance are retained locally at `.playwright-cli/tunnel-qa/full/{report,provenance}.json`. The complete Claude summary is `/tmp/tunnel-claude-qa-summary.md`. After the full run, two poller findings were fixed: contention no longer changes diagnostic state; a 404 disables polling for the session. The focused final-poller rerun (`20260928T140112Z`) passed 8/8: old/new/opted-out agents against the new gateway, four faults, and near-full. The added 404 fault verified exactly one poll and `unsupported` state. Reports are retained in `.playwright-cli/tunnel-qa/final-poller/`. Subsequent metrics/cancellation fixes do not change the poller; rebuilt-source smoke and explicit process rollback checks are recorded separately below.

## Final-source smoke and actual rollback

Run `20260928T141747Z` passed **4/4**, all from one build of the final tunnel code (`587fd9a07220f13729d7cbb676f12d6f764a232edeb8c713d9935d1ed684e514` tunnel diff hash):

- New agent + frozen old gateway: full pair checks passed.
- New agent + new gateway: full pair checks passed, including 40 seconds idle with three TCP probes and zero HTTP probes.
- **Gateway replacement:** new → frozen old → new on the same public address, without replacing the agent process. Re-admission took 0.2–0.9 seconds; every forwarding check returned 200. The old gateway produced the legacy snapshot, and new gateways resumed diagnostics.
- **Agent replacement:** new agent → published 0.1.0 image against the same running new gateway. The old image admitted in 0.5 seconds, forwarded successfully and reported `unsupported` diagnostics without a new target display/report.

Reports/provenance are retained in `.playwright-cli/tunnel-qa/rollback/`. Final new agent SHA-256: `527bae7d16a3b3fb459397f57d3e0fe23ad85a2f408165dec0698502b81beaeb`; new gateway: `d217fa3fbbffc092265bd05902e8582336298620175de5b680b3289d673e17d9`. The old-image digest is unchanged. Codex independently rebuilt the current agent and compatibility gateway and reproduced both tested binary hashes exactly. Its recomputed composite text-diff hash differed from the report for an undetermined reason; that evidence caveat is retained, and exact binary reproduction establishes functional provenance. The compatibility README documents how to run these transitions.

Earlier attempts are retained in Claude's scratchpad: one Docker process never started (container stayed Created), and one harness assertion rejected expected EOF messages during intentional process replacement. The final harness permits those teardown messages without weakening forwarding/admission checks. No production workaround was made for either harness/environment failure.

## Other verification

| Check                                                          | Result                                                                            | Local evidence                                                                   |
| -------------------------------------------------------------- | --------------------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| Server suites: tunneledmcp, tunnelmetrics, proxy, mv, mcp      | 966 tests passed                                                                  | `/tmp/tunnel-tests-final.log`                                                    |
| Added retry, policy-rejection and idle/loss coverage cases     | 206 tests passed                                                                  | `/tmp/tunnel-new-tests.log`                                                      |
| Receipt-age, stale state and two-failure UI hysteresis         | 30 tests passed                                                                   | `/tmp/tunnel-mv-final.log`                                                       |
| Agent/gateway/collector/wire race tests                        | PASS                                                                              | `/tmp/tunnel-race-final.log`                                                     |
| All tunnel packages, including existing e2e                    | PASS                                                                              | `/tmp/tunnel-all-final.log`                                                      |
| Real ClickHouse latest-revision/out-of-order dedup and scoping | PASS, included above                                                              | `server/internal/tunnelmetrics/store_test.go`                                    |
| Demo seed isolation and postflight checks                      | 12 tests passed                                                                   | `/tmp/tunnel-seed-tests2.log`                                                    |
| Server lint                                                    | PASS; existing deprecated-linter warning                                          | `/tmp/tunnel-lint-final.log`                                                     |
| Dashboard type check                                           | PASS                                                                              | `/tmp/tunnel-ts-final.log`                                                       |
| Agent-only Docker build                                        | PASS; `--version` gives `0.2.0-dev`                                               | `/tmp/tunnel-agent-image.log`                                                    |
| Desktop/mobile browser checks                                  | PASS after fixing title/tile overflow; mobile document has no horizontal overflow | `.playwright-cli/tunnel-*-final.png`, `.playwright-cli/tunnel-diagnostics-*.png` |
| Mechanical UI detector                                         | No findings                                                                       | `/tmp/tunnel-ui-detect.json`                                                     |

The bounded collector test creates 10,000 sources, holds all 16 publisher workers, verifies recording continues without waiting for publication, and cancels cleanly. This is a local resource-bound check, not a fleet throughput/SLO benchmark.

A real synthetic target and 0.2.0-dev agent were connected to the isolated local gateway and API. The management API and dashboard showed one connected agent, transport reachable, DNS/TLS not applicable for a literal HTTP loopback target, and only the permitted target URL components. A real `tools/list` request succeeded through the MCP endpoint; after typed publication and batch ingestion, history contained one attempt, one success, a 10ms histogram upper bound, one producer heartbeat and current gateway samples. Thus the producer → Pub/Sub emulator → streams → ClickHouse → authorized API chain was exercised, not just seed data.

Additional review-driven validation: `/tmp/tunnel-review-tests.log` passed 53 tests, including authoritative mixed-project ingestion, unknown/deleted source filtering and retryable ownership lookup failures. `/tmp/tunnel-review-race.log`, `/tmp/tunnel-review-lint.log`, `/tmp/tunnel-dashboard-lint-final.log` (Oxlint) and `/tmp/tunnel-ts-reviewed.log` have explicit `exit=0` markers. `/tmp/tunnel-agent-image-final.log` records the rebuilt final agent image, also exit 0. Counts in this report are suite totals, not numbers of newly added tests.

A live target stop/restart exercise kept one agent session connected throughout. The API moved reachable → first failure unknown → unreachable with `tcp_refused` → reachable after target restart. The raw local evidence is `/tmp/tunnel-recovery-e2e.log`, with a repeat on final code in `/tmp/tunnel-recovery-final.log`. After the agent exited, `/tmp/tunnel-disconnected-zero.json` confirmed zero live connections and an observed zero-connection history bucket with coverage, rather than a null gap.

All `.playwright-cli/` and `/tmp/` evidence is local and gitignored/temporary. The tracked summaries here preserve results and provenance; the compatibility harness and tests reproduce them in another checkout. Screenshots are review captures, not permanent production monitoring.

## Historical baseline completion review

Two fresh reviewers were launched in separate visible Herdr panels with focus preserved and the same standalone requirements/evidence task, without the implementing conversation:

- Claude via `claude-danger`: **APPROVED. Complete for local delivery and a flagged canary rollout. No material findings remain.** Full local review: `/tmp/tunnel-claude-completion-review.md`.
- Codex via `codex-danger`: **PASS. Requested local implementation, validation, explicit rollback and standalone QA handoff are complete; no material defect or completion blocker remains.** Full local review: `/tmp/tunnel-codex-completion-review.md`.

Resolved findings: two-failure target-state threshold, live hero/source status labels, explicit `too_large` history feedback, five-minute disconnected-source observation, authoritative ingest tests, visible active-server count, cancellation excluded from target transport errors, import grouping, actual downgrade evidence and final captures. The fresh Claude re-review also ran 239 server tests successfully. Dashboard uses Oxlint, not ESLint; final Oxlint and type-check passed. Final captures at 15:19–15:20 BST show the implemented UI after the functional fixes, with no horizontal mobile overflow. The owner-restricted HTTPS review preview was verified and all prior Tailscale routes, including 443 and 8443, were preserved.

Accepted non-blocking limits are listed below and in the implementation doc: real IAM/credential refresh, fleet query/collector capacity, bounded disconnect coverage, same-slot restart sampling ambiguity and customer network/routing canaries. Existing source-page permission/live-label/responsive fixes apply outside the new chart flag and are disclosed in the changeset. Build-version defaults and previous diagnostic reports retained under unavailable state are documented. No Claude quota fallback or paid overage has been used.

## Limits

The old-source fixture and published 0.1.0 image are concrete compatibility evidence; they are not an inventory of every deployed build. Old-agent 404 polls are not directly observable, and the noaccept fault has no deliberately broken poller negative control. Full-fleet capacity, production IAM/ingress redaction, all real deployment routing combinations, and customer-specific DNS/proxy/TLS networks remain canary release gates. History is best effort, bounded and seven-day retained. Known disconnected sources are sampled as zero for a five-minute lease, then become unknown. Overlarge history reads return `too_large` with a shorter-range instruction; query aggregation at larger fleet scale remains a release gate. Same-slot gateway restarts and missing owners limit sampled concurrency precision; charts disclose observed coverage. None of these diagnostics control routing or admission.

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
target summary to “Not checked”. Accepted limitation: producers do not drain
in-memory aggregates on shutdown; the plan now explicitly describes recent and
pending-retry loss, including the absence of a prior-boot loss watermark. This
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

Fresh independent completion reviewers ran via `claude-danger` and
`codex-danger` in visible, unfocused Herdr panels, using standalone requirements
and current artifacts. Claude's final verdict: **implementation and verification
complete, ready for PR; no remaining code, privacy, compatibility or guidance
defect**. It independently ran 743 server tests, all tunnel tests and vet,
four UI tests, dashboard type checking, Oxlint and formatting. Codex's code
re-review found no remaining implementation findings after the cached-history
color regression and Platform MCP assessment were resolved; final compatibility
and delivery review is retained in `/tmp/tunnel-current-codex-review.md`.
Claude's full review is `/tmp/tunnel-current-claude-review.md`.

PR screenshots show synthetic seeded history with a real local agent/target;
the local session prefix is redacted. Only the clean overview and target-failure
crops are published. Mobile layout was checked but its dev-overlay capture is
not a public demo. Production image publication, IAM, customer-network canaries
and fleet-scale capacity remain release gates; PR publication does not claim
those completed. No quota fallback or paid overage was used.
