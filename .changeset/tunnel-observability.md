---
"tunnel": minor
"server": minor
"dashboard": minor
---

Add optional, payload-free tunnel diagnostics and activity history. Existing agents continue forwarding unchanged. New agents report target transport checks when a capable gateway requests them, and support an explicit diagnostics opt-out. The dashboard separates tunnel connectivity, target reachability, and MCP activity.

Report aggregate waiting-header/open-response gauges every 30 seconds without parsing payloads or issuing MCP requests. Keep idle HTTP/MCP evidence unobserved, defer collapsed-assistant discovery, and provide five passive Docker Compose fixtures for the real Gram Overview.
