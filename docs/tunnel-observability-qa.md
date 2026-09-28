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
