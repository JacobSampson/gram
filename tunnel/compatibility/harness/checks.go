package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Wire header names are spelled out rather than imported so the harness does
// not depend on either tree's wire package.
const (
	hdrTunnelID        = "X-Gram-Tunnel-Id"
	hdrForwardToken    = "X-Gram-Tunnel-Forward-Token"
	hdrConsumerSession = "X-Gram-Tunnel-Consumer-Session"
	hdrAgentSession    = "X-Gram-Tunnel-Agent-Session"
	hdrTunnelError     = "X-Gram-Tunnel-Error"
)

// legacyConnectionKeys is route.Connection's JSON shape at the old baseline.
var legacyConnectionKeys = []string{
	"gateway_session_id", "service_version", "agent_version", "connected_at", "last_heartbeat_at",
	"remote_addr", "active_substreams", "active_consumer_sessions", "metadata",
}

type checker struct {
	o       options
	as      agentSpec
	gs      gatewaySpec
	up      *upstream
	gw      *gatewayProc
	secrets pairSecrets
	add     func(name string, pass bool, format string, args ...any)
	info    func(name, format string, args ...any)
	facts   map[string]any

	mu            sync.Mutex
	agentSessions map[string]int
}

// client bounds every forward, including the soak's long-lived stream, so the
// timeout must outlast the longest requested soak.
func (c *checker) client() *http.Client {
	return &http.Client{Timeout: max(60*time.Second, c.o.soak+30*time.Second, c.o.nearFullHold+30*time.Second)}
}

func (c *checker) forward(ctx context.Context, method, path string, body io.Reader, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.gw.ready.ForwardAddr+path, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set(hdrTunnelID, c.secrets.tunnelID)
	req.Header.Set(hdrForwardToken, c.secrets.forwardToken)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if s := resp.Header.Get(hdrAgentSession); s != "" {
		c.mu.Lock()
		if c.agentSessions == nil {
			c.agentSessions = map[string]int{}
		}
		c.agentSessions[s]++
		c.mu.Unlock()
	}
	return resp, nil
}

func readAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *checker) requestsSince(t time.Time, pathPrefix string) []upstreamRequest {
	var out []upstreamRequest
	for _, r := range c.up.requestsSnapshot() {
		if !r.At.Before(t) && strings.HasPrefix(r.Path, pathPrefix) {
			out = append(out, r)
		}
	}
	return out
}

func (c *checker) run(ctx context.Context, admitted gatewayState) {
	c.checkSnapshot(admitted)
	c.checkJSONEcho(ctx)
	c.checkStatusPassthrough(ctx)
	c.checkRootPin(ctx)
	c.checkOAuthPaths(ctx)
	c.checkLargeUpload(ctx)
	c.checkLargeDownload(ctx)
	c.checkSSE(ctx)
	c.checkConcurrency(ctx)
	c.checkControlIsolation(ctx)
	c.checkSoak(ctx)
	c.checkJSONEchoNamed(ctx, "post_soak_echo")
	c.checkSessionPinning()
	c.checkDiagnostics(ctx)
}

// checkSnapshot verifies the legacy connection projection is intact and that
// the agent carried nothing extra in the free-form metadata map.
func (c *checker) checkSnapshot(st gatewayState) {
	conns := st.connections()
	if len(conns) != 1 {
		c.add("snapshot_legacy_fields", false, "expected 1 connection, got %d", len(conns))
		return
	}
	conn := conns[0]
	var missing, extra []string
	for _, k := range legacyConnectionKeys {
		if _, ok := conn[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range conn {
		if !slices.Contains(legacyConnectionKeys, k) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	c.facts["snapshot_extra_keys"] = extra
	c.add("snapshot_legacy_fields", len(missing) == 0, "missing=%v extra=%v", missing, extra)
	if c.gs.Old && len(extra) > 0 {
		c.add("snapshot_old_gateway_shape", false, "old gateway projected unexpected keys %v", extra)
	}

	var metadata map[string]string
	_ = json.Unmarshal(conn["metadata"], &metadata)
	c.add("snapshot_metadata_unchanged", len(metadata) == 1 && metadata["compat"] == "harness",
		"metadata=%v (must equal configured TUNNEL_METADATA only)", metadata)

	var serviceVersion, agentVersion string
	_ = json.Unmarshal(conn["service_version"], &serviceVersion)
	_ = json.Unmarshal(conn["agent_version"], &agentVersion)
	c.facts["agent_version"] = agentVersion
	// Frozen agents report the baseline 0.1.0; new agents must not reuse it.
	versionOK := agentVersion != "" && (c.as.Old == (agentVersion == "0.1.0"))
	c.add("snapshot_versions", serviceVersion == "compat-"+c.as.Kind && versionOK,
		"service_version=%q agent_version=%q", serviceVersion, agentVersion)
}

func (c *checker) checkJSONEcho(ctx context.Context) { c.checkJSONEchoNamed(ctx, "json_echo") }

func (c *checker) checkJSONEchoNamed(ctx context.Context, name string) {
	started := time.Now()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{"secret":%q}}}`, c.secrets.sentinel)
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Accept", "application/json, text/event-stream")
	hdr.Set("Authorization", c.secrets.upstreamAuth)
	hdr.Set("X-Compat-Req", "req-value")
	hdr.Set("Mcp-Session-Id", "client-session")
	hdr.Set(hdrConsumerSession, "consumer-affinity")
	resp, err := c.forward(ctx, http.MethodPost, "/echo?alpha=1&beta=two%20words", strings.NewReader(body), hdr)
	if err != nil {
		c.add(name, false, "forward: %v", err)
		return
	}
	raw, err := readAll(resp)
	if err != nil {
		c.add(name, false, "read: %v", err)
		return
	}
	var problems []string
	if resp.StatusCode != http.StatusOK {
		problems = append(problems, fmt.Sprintf("status %d (%s) body %q", resp.StatusCode, resp.Header.Get(hdrTunnelError), raw))
	}
	var echo echoResponse
	if err := json.Unmarshal(raw, &echo); err != nil {
		problems = append(problems, "decode echo: "+err.Error())
	}
	if echo.Body != body {
		problems = append(problems, "body not preserved")
	}
	// ReverseProxy prefixes the pinned target's own query, in every version.
	wantQuery := "alpha=1&beta=two%20words"
	if c.secrets.target.enabled() {
		wantQuery = c.secrets.target.query + "&" + wantQuery
	}
	if echo.Path != c.as.targetPath("/echo") || echo.RawQuery != wantQuery || echo.Method != http.MethodPost {
		problems = append(problems, fmt.Sprintf("request line changed: %s %s ? %s", echo.Method, echo.Path, echo.RawQuery))
	}
	if echo.Headers["X-Compat-Req"] != "req-value" {
		problems = append(problems, "custom request header lost")
	}
	if resp.Header.Get("X-Compat-Upstream") != "yes" || strings.Join(resp.Header.Values("X-Compat-Multi"), ",") != "a,b" {
		problems = append(problems, "response headers not preserved")
	}
	if resp.Header.Get(hdrForwardToken) != "" {
		problems = append(problems, "forward token echoed in response")
	}
	if resp.Header.Get(hdrAgentSession) == "" {
		problems = append(problems, "gateway did not report agent session")
	}
	reqs := c.requestsSince(started, c.as.targetPath("/echo"))
	if len(reqs) != 1 {
		problems = append(problems, fmt.Sprintf("upstream saw %d echo requests, want 1", len(reqs)))
	} else {
		got := reqs[0].Header
		if got.Get("Authorization") != c.secrets.upstreamAuth || got.Get("Mcp-Session-Id") != "client-session" {
			problems = append(problems, "authorization or MCP session header not forwarded")
		}
		for _, h := range []string{hdrForwardToken, hdrTunnelID, hdrConsumerSession, hdrAgentSession} {
			if got.Get(h) != "" {
				problems = append(problems, "internal header reached target: "+h)
			}
		}
		for h := range got {
			if strings.HasPrefix(strings.ToLower(h), "x-gram-") {
				problems = append(problems, "x-gram header reached target: "+h)
			}
		}
	}
	c.add(name, len(problems) == 0, "%s", joinOr(problems, "method, path, query, headers, body and status preserved"))
}

func (c *checker) checkStatusPassthrough(ctx context.Context) {
	var problems []string
	for _, status := range []int{http.StatusCreated, http.StatusUnauthorized, http.StatusTeapot, http.StatusInternalServerError, http.StatusBadGateway} {
		resp, err := c.forward(ctx, http.MethodGet, fmt.Sprintf("/echo?status=%d", status), nil, nil)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		raw, readErr := readAll(resp)
		var echo echoResponse
		wantQuery := fmt.Sprintf("status=%d", status)
		if c.secrets.target.enabled() {
			wantQuery = c.secrets.target.query + "&" + wantQuery
		}
		if readErr != nil || resp.StatusCode != status || json.Unmarshal(raw, &echo) != nil || resp.Header.Get(hdrTunnelError) != "" || echo.Method != http.MethodGet || echo.Path != c.as.targetPath("/echo") || echo.RawQuery != wantQuery || echo.Body != "" || resp.Header.Get("X-Compat-Upstream") != "yes" {
			problems = append(problems, fmt.Sprintf("want %d got %d tunnel-error=%q", status, resp.StatusCode, resp.Header.Get(hdrTunnelError)))
		}
	}
	c.add("status_passthrough", len(problems) == 0, "%s", joinOr(problems, "2xx/4xx/5xx target statuses and bodies relayed unchanged"))
}

func (c *checker) checkRootPin(ctx context.Context) {
	started := time.Now()
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Accept", "application/json, text/event-stream")
	resp, err := c.forward(ctx, http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"initialize"}`), hdr)
	if err != nil {
		c.add("root_path_pinned", false, "%v", err)
		return
	}
	raw, _ := readAll(resp)
	reqs := c.requestsSince(started, "/")
	ok := resp.StatusCode == http.StatusOK && len(reqs) == 1 && reqs[0].Path == upstreamBasePath &&
		strings.Contains(string(raw), `"id":7`) && resp.Header.Get("Mcp-Session-Id") == "compat-session"
	c.add("root_path_pinned", ok, "status=%d upstream_paths=%v body=%s", resp.StatusCode, paths(reqs), truncate(raw, 120))
}

// checkOAuthPaths forwards issuer back-channel requests on their own non-root
// paths, as gram-server does for an OAuth issuer bound to the tunnel. 0.1.0
// joins them beneath the pinned MCP path; 0.1.1 and later deliver them on the
// request's own path, with the pinned query first and an escaped segment kept.
func (c *checker) checkOAuthPaths(ctx context.Context) {
	c.facts["path_routing"] = c.as.routing()
	pinnedQuery := ""
	if c.secrets.target.enabled() {
		pinnedQuery = c.secrets.target.query + "&"
	}
	cases := []struct {
		method, path, query, wantRaw string
		body                         string
	}{
		{http.MethodPost, oauthTokenPath, "client=compat", "", "grant_type=authorization_code&code=" + c.secrets.sentinel},
		{http.MethodGet, oauthMetadataPath + "/tenant%2Fa", "", oauthMetadataPath + "/tenant%2Fa", ""},
	}
	var problems, seen []string
	for _, tc := range cases {
		started := time.Now()
		hdr := http.Header{}
		var body io.Reader
		if tc.body != "" {
			hdr.Set("Content-Type", "application/x-www-form-urlencoded")
			body = strings.NewReader(tc.body)
		}
		target := tc.path
		if tc.query != "" {
			target += "?" + tc.query
		}
		resp, err := c.forward(ctx, tc.method, target, body, hdr)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", tc.method, tc.path, err))
			continue
		}
		raw, _ := readAll(resp)
		var echo echoResponse
		_ = json.Unmarshal(raw, &echo)
		wantRaw := c.as.targetPath(tc.path)
		wantPath, _ := url.PathUnescape(wantRaw)
		wantQuery := strings.TrimSuffix(pinnedQuery+tc.query, "&")
		seen = append(seen, echo.RawPath)
		switch {
		case resp.StatusCode != http.StatusOK:
			problems = append(problems, fmt.Sprintf("%s %s: status %d upstream_paths=%v", tc.method, tc.path, resp.StatusCode, paths(c.requestsSince(started, ""))))
		case echo.Method != tc.method || echo.Path != wantPath || echo.RawQuery != wantQuery || echo.Body != tc.body:
			problems = append(problems, fmt.Sprintf("%s %s: target saw %s %s ? %s", tc.method, tc.path, echo.Method, echo.Path, echo.RawQuery))
		case tc.wantRaw != "" && echo.RawPath != wantRaw:
			problems = append(problems, fmt.Sprintf("%s %s: escaped path %s, want %s", tc.method, tc.path, echo.RawPath, wantRaw))
		}
	}
	c.add("oauth_nonroot_path", len(problems) == 0, "%s", joinOr(problems, fmt.Sprintf("%s routing; target saw %v", c.as.routing(), seen)))
}

func (c *checker) checkLargeUpload(ctx context.Context) {
	const size = 8 << 20
	seed := "upload-" + randomHex(4)
	want := patternSHA256(seed, size)
	resp, err := c.forward(ctx, http.MethodPost, "/sink", io.LimitReader(newPatternReader(seed), size), http.Header{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		c.add("large_upload_8MiB", false, "%v", err)
		return
	}
	raw, _ := readAll(resp)
	var got struct {
		SHA256 string `json:"sha256"`
		Len    int64  `json:"len"`
	}
	_ = json.Unmarshal(raw, &got)
	c.add("large_upload_8MiB", resp.StatusCode == 200 && got.SHA256 == want && got.Len == size,
		"status=%d len=%d sha_match=%t", resp.StatusCode, got.Len, got.SHA256 == want)
}

func (c *checker) checkLargeDownload(ctx context.Context) {
	const size = 8 << 20
	seed := "download-" + randomHex(4)
	want := patternSHA256(seed, size)
	resp, err := c.forward(ctx, http.MethodGet, fmt.Sprintf("/blob?size=%d&seed=%s", size, seed), nil, nil)
	if err != nil {
		c.add("large_download_8MiB", false, "%v", err)
		return
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	got := hex.EncodeToString(h.Sum(nil))
	c.add("large_download_8MiB", err == nil && resp.StatusCode == 200 && n == size && got == want,
		"status=%d len=%d sha_match=%t err=%v", resp.StatusCode, n, got == want, err)
}

// checkSSE proves every hop streams: the first event must arrive while the
// target is still blocked before writing the second.
func (c *checker) checkSSE(ctx context.Context) {
	id := randomHex(6)
	hdr := http.Header{"Accept": {"text/event-stream"}}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.forward(ctx, http.MethodGet, "/sse?id="+id, nil, hdr)
	if err != nil {
		c.add("sse_streaming", false, "%v", err)
		return
	}
	defer resp.Body.Close()
	events := make(chan string, 4)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				events <- data
			}
		}
	}()
	var first string
	select {
	case first = <-events:
	case <-time.After(5 * time.Second):
		c.add("sse_streaming", false, "first event not delivered within 5s while target held the stream (buffering?)")
		c.up.release(id)
		return
	}
	c.up.release(id)
	var rest []string
	for e := range events {
		rest = append(rest, e)
	}
	ok := resp.StatusCode == 200 && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") &&
		first == "one" && slices.Equal(rest, []string{"two"})
	c.add("sse_streaming", ok, "status=%d first=%q rest=%v", resp.StatusCode, first, rest)
}

func (c *checker) checkConcurrency(ctx context.Context) {
	const n = 64
	var wg sync.WaitGroup
	errs := make(chan string, n)
	begin := time.Now()
	for i := range n {
		wg.Go(func() {
			body := fmt.Sprintf(`{"i":%d,"pad":%q}`, i, strings.Repeat("x", 4096))
			resp, err := c.forward(ctx, http.MethodPost, fmt.Sprintf("/echo?i=%d", i), strings.NewReader(body), nil)
			if err != nil {
				errs <- err.Error()
				return
			}
			raw, _ := readAll(resp)
			var echo echoResponse
			if resp.StatusCode != 200 || json.Unmarshal(raw, &echo) != nil || echo.Body != body {
				errs <- fmt.Sprintf("req %d: status %d", i, resp.StatusCode)
			}
		})
	}
	wg.Wait()
	close(errs)
	var problems []string
	for e := range errs {
		problems = append(problems, e)
	}
	c.facts["concurrency_64_ms"] = time.Since(begin).Milliseconds()
	c.add("concurrency_64", len(problems) == 0, "%s", joinOr(problems, fmt.Sprintf("64 parallel forwards ok in %s", time.Since(begin).Round(time.Millisecond))))
}

// checkControlIsolation sends consumer-controlled requests to reserved control
// paths. None may reach the target, and unauthenticated status must be 404.
func (c *checker) checkControlIsolation(ctx context.Context) {
	started := time.Now()
	statuses := map[string]int{}
	probes := []struct{ method, path string }{
		{http.MethodGet, "/_tunnel/status"},
		{http.MethodPost, "/_tunnel/status"},
		{http.MethodGet, "/_tunnel/unknown"},
		{http.MethodPost, "/_tunnel/hello"},
	}
	var problems []string
	for _, p := range probes {
		resp, err := c.forward(ctx, p.method, p.path, strings.NewReader(`{}`), http.Header{"Content-Type": {"application/json"}})
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", p.method, p.path, err))
			continue
		}
		_, _ = readAll(resp)
		statuses[p.method+" "+p.path] = resp.StatusCode
	}
	for _, k := range []string{"GET /_tunnel/status", "POST /_tunnel/status", "GET /_tunnel/unknown", "POST /_tunnel/hello"} {
		want := http.StatusNotFound
		// Frozen gateways forward the legacy hello handshake to the agent.
		// New gateways block consumer access to all reserved control paths.
		if c.gs.Old && k == "POST /_tunnel/hello" {
			want = http.StatusOK
		}
		if statuses[k] != want {
			problems = append(problems, fmt.Sprintf("%s -> %d, want %d", k, statuses[k], want))
		}
	}
	for _, r := range c.requestsSince(started, "") {
		if strings.Contains(r.Path, "_tunnel") {
			problems = append(problems, "control path reached target: "+r.Path)
		}
	}
	c.facts["control_path_statuses"] = statuses
	c.add("control_path_isolation", len(problems) == 0, "%s", joinOr(problems, fmt.Sprintf("statuses %v; nothing reached target", statuses)))
}

// checkSoak holds one long-lived stream open through an otherwise idle window
// spanning keepalives and (for new/new) diagnostic polls. It verifies stream
// continuity, a single stable session, and which traffic reached the target.
func (c *checker) checkSoak(ctx context.Context) {
	count := int(c.o.soak/c.o.tick) + 1
	started := time.Now()
	resp, err := c.forward(ctx, http.MethodGet, fmt.Sprintf("/ticker?count=%d&interval=%s", count, c.o.tick), nil, http.Header{"Accept": {"text/event-stream"}})
	if err != nil {
		c.add("soak_long_stream", false, "%v", err)
		return
	}
	// The ticker's own upstream connection is accepted just before this point.
	windowStart := time.Now()
	stopSampling := make(chan struct{})
	samples := make(chan string, 1024)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopSampling:
				close(samples)
				return
			case <-t.C:
				st, err := c.gw.state(ctx)
				switch {
				case err != nil:
					samples <- "state error: " + err.Error()
				case st.ActiveSessions != 1 || len(st.Candidates) != 1:
					samples <- fmt.Sprintf("active=%d candidates=%d", st.ActiveSessions, len(st.Candidates))
				}
			}
		}
	}()

	var got []string
	var maxGap time.Duration
	last := time.Now()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			now := time.Now()
			if len(got) > 0 {
				maxGap = max(maxGap, now.Sub(last))
			}
			last = now
			got = append(got, data)
		}
	}
	_ = resp.Body.Close()
	windowEnd := time.Now()
	close(stopSampling)
	var stateProblems []string
	for s := range samples {
		stateProblems = append(stateProblems, s)
	}

	want := make([]string, count)
	for i := range want {
		want[i] = fmt.Sprintf("tick-%d", i)
	}
	streamOK := resp.StatusCode == 200 && slices.Equal(got, want) && maxGap <= c.o.tick+3*time.Second
	c.add("soak_long_stream", streamOK, "status=%d events=%d/%d max_gap=%s over %s",
		resp.StatusCode, len(got), count, maxGap.Round(time.Millisecond), windowEnd.Sub(started).Round(time.Second))
	c.add("soak_session_stable", len(stateProblems) == 0, "%s", joinOr(dedupe(stateProblems), "1 live session and route at every 1s sample"))

	var idleReqs []string
	for _, r := range c.up.requestsSnapshot() {
		if r.At.After(windowStart) && r.At.Before(windowEnd) && r.Path != c.as.targetPath("/ticker") {
			idleReqs = append(idleReqs, r.Method+" "+r.Path)
		}
	}
	idleConns := 0
	var idleRemotes []string
	for _, cn := range c.up.connsSnapshot() {
		if cn.At.After(windowStart) && cn.At.Before(windowEnd) {
			idleConns++
			if len(idleRemotes) < 20 {
				idleRemotes = append(idleRemotes, cn.At.Format("15:04:05.000")+" "+cn.RemoteAddr)
			}
		}
	}
	c.facts["idle_target_connections"] = idleConns
	c.facts["idle_target_connection_sample"] = idleRemotes
	c.facts["idle_target_requests"] = idleReqs
	// Diagnostics may open TCP/TLS to the target but must never send HTTP.
	c.add("idle_no_target_http", len(idleReqs) == 0, "%s", joinOr(idleReqs, "no HTTP requests reached target while idle"))
	if c.diagnosticsExpected() {
		c.info("idle_target_connections", "%d new TCP connections to target during %s idle window (diagnostic probes expected)", idleConns, c.o.soak)
	} else {
		c.add("idle_no_target_probes", idleConns == 0, "%d new TCP connections to target during idle window; want 0 when diagnostics are not negotiated", idleConns)
	}
}

// diagnosticsExpected is true only when both sides support diagnostics and the
// agent has not opted out.
func (c *checker) diagnosticsExpected() bool {
	enabled := true
	for _, entry := range c.gs.Env {
		if value, ok := strings.CutPrefix(entry, "TUNNEL_DIAGNOSTICS_ENABLED="); ok {
			value = strings.ToLower(strings.TrimSpace(value))
			enabled = value != "0" && value != "false"
		}
	}
	return c.as.Diagnostics && !c.gs.Old && enabled
}

func (c *checker) checkSessionPinning() {
	c.mu.Lock()
	sessions := make([]string, 0, len(c.agentSessions))
	for s := range c.agentSessions {
		sessions = append(sessions, s)
	}
	c.mu.Unlock()
	c.add("single_agent_session", len(sessions) == 1, "distinct agent sessions reported on forwards: %d", len(sessions))
}

// checkDiagnostics inspects the projected connection after the soak. Only a
// negotiated new/new session may carry diagnostics or a target display; every
// other pair must project the legacy shape (or an explicit unsupported state).
func (c *checker) checkDiagnostics(ctx context.Context) {
	st, err := c.gw.state(ctx)
	if err != nil || len(st.connections()) != 1 {
		c.add("diagnostics_projection", false, "state: err=%v connections=%d", err, len(st.connections()))
		return
	}
	conn := st.connections()[0]
	extra := map[string]json.RawMessage{}
	for k, v := range conn {
		if !slices.Contains(legacyConnectionKeys, k) {
			extra[k] = v
		}
	}
	rendered, _ := json.Marshal(extra)
	c.facts["diagnostics_fields_after_soak"] = json.RawMessage(rendered)

	var leaks []string
	whole, _ := json.Marshal(conn)
	for label, secret := range c.allSecrets() {
		if bytes.Contains(whole, []byte(secret)) {
			leaks = append(leaks, label)
		}
	}
	c.add("snapshot_no_zero_timestamps", !bytes.Contains(whole, []byte("0001-01-01T00:00:00Z")), "zero-value timestamps must be omitted")
	c.add("snapshot_no_secrets", len(leaks) == 0, "%s", joinOr(leaks, "no request, key or target secret in the projected connection"))

	var diag struct {
		State  string `json:"state"`
		Report *struct {
			Sequence    int64  `json:"sequence"`
			TargetState string `json:"target_state"`
			SampleAge   int64  `json:"sample_age_ms"`
			DNS         struct {
				State string `json:"state"`
			} `json:"dns"`
			TCP struct {
				State string `json:"state"`
			} `json:"tcp"`
			TLS struct {
				State string `json:"state"`
			} `json:"tls"`
		} `json:"report"`
	}
	hasDiag := len(extra["diagnostics"]) > 0 && string(extra["diagnostics"]) != "null"
	if hasDiag {
		_ = json.Unmarshal(extra["diagnostics"], &diag)
	}
	var display string
	_ = json.Unmarshal(extra["target_display"], &display)

	switch {
	case c.gs.Old:
		c.add("diagnostics_projection", len(extra) == 0, "old gateway: extra=%s", rendered)
	case !c.diagnosticsExpected():
		// A capable agent on a gateway with collection off is "disabled" and
		// still carries its sanitized display; anything else is "unsupported".
		wantState, wantDisplay := "unsupported", ""
		if c.as.Diagnostics {
			wantState = "disabled"
			if c.as.Image == "" {
				wantDisplay = "http://" + c.up.addr() + upstreamBasePath
			}
		}
		ok := hasDiag && diag.State == wantState && diag.Report == nil && display == wantDisplay
		c.add("diagnostics_explicit_state", ok, "state=%q (want %q) display=%q report=%t", diag.State, wantState, display, diag.Report != nil)
	case c.o.requireDiag:
		var problems []string
		if !hasDiag || diag.State != "available" || diag.Report == nil {
			problems = append(problems, fmt.Sprintf("diagnostics state %q report=%t", diag.State, diag.Report != nil))
		} else {
			r := diag.Report
			if r.Sequence < 1 || r.TargetState != "reachable" || r.SampleAge < 0 {
				problems = append(problems, fmt.Sprintf("sample not completed: sequence=%d target_state=%s sample_age_ms=%d", r.Sequence, r.TargetState, r.SampleAge))
			}
			// The pinned target is a literal IP over plain HTTP when run natively.
			if c.as.Image == "" && (r.DNS.State != "not_applicable" || r.TCP.State != "pass" || r.TLS.State != "not_applicable") {
				problems = append(problems, fmt.Sprintf("steps dns=%s tcp=%s tls=%s, want not_applicable/pass/not_applicable", r.DNS.State, r.TCP.State, r.TLS.State))
			}
		}
		var times struct {
			AttemptedAt time.Time `json:"attempted_at"`
			ReceivedAt  time.Time `json:"received_at"`
		}
		_ = json.Unmarshal(extra["diagnostics"], &times)
		if lag := times.ReceivedAt.Sub(times.AttemptedAt); !times.AttemptedAt.Before(times.ReceivedAt) || lag > 5*time.Second {
			problems = append(problems, fmt.Sprintf("attempted_at %s must precede received_at %s by <5s", times.AttemptedAt, times.ReceivedAt))
		}
		wantDisplay := "http://" + c.up.addr() + upstreamBasePath
		if c.as.Image == "" && display != wantDisplay {
			problems = append(problems, fmt.Sprintf("target_display %q, want %q", display, wantDisplay))
		}
		c.add("diagnostics_reported", len(problems) == 0, "%s", joinOr(problems, "available, reachable, completed sample; sanitized target display"))
	default:
		c.info("diagnostics_projection", "extra=%s", truncate(rendered, 600))
	}
}

func (c *checker) allSecrets() map[string]string {
	out := map[string]string{
		"tunnel key": c.secrets.tunnelKey, "forward token": c.secrets.forwardToken,
		"request payload": c.secrets.sentinel, "upstream credential": c.secrets.upstreamAuth,
	}
	maps.Copy(out, c.secrets.target.secrets())
	return out
}

// checkLogs scans steady-state logs for disconnects, warnings and secrets.
func (c *checker) checkLogs(gwLog, agentLog string, ag *proc) {
	gwLines, gwRaw, gwErr := readLog(gwLog)
	agLines, agRaw, agErr := readLog(agentLog)
	if gwErr != nil || agErr != nil {
		c.add("logs_readable", false, "gateway=%v agent=%v", gwErr, agErr)
		return
	}
	connected := countMsg(gwLines, "tunnel connected")
	disconnected := countMsg(gwLines, "tunnel disconnected")
	agConnected := countMsg(agLines, "tunnel agent connected")
	agEnded := countMsg(agLines, "tunnel agent session ended") + countMsg(agLines, "tunnel agent reconnecting")
	c.add("connection_continuity", connected == 1 && disconnected == 0 && agConnected == 1 && agEnded == 0 && !ag.exited(),
		"gateway connected=%d disconnected=%d; agent connected=%d ended/reconnecting=%d exited=%t",
		connected, disconnected, agConnected, agEnded, ag.exited())

	var warnings []string
	for _, set := range []struct {
		name  string
		lines []logLine
	}{{"gateway", gwLines}, {"agent", agLines}} {
		for _, l := range set.lines {
			if strings.EqualFold(l.Level, "WARN") || strings.EqualFold(l.Level, "ERROR") {
				warnings = append(warnings, set.name+": "+l.Msg)
			}
		}
	}
	c.facts["steady_state_warnings"] = warnings
	c.add("no_steady_state_warnings", len(warnings) == 0, "%s", joinOr(dedupe(warnings), "no WARN/ERROR lines (yamux debug output enabled)"))

	var leaks, legacyLeaks []string
	targetSecrets := c.secrets.target.secrets()
	for name, raw := range map[string]string{"gateway": gwRaw, "agent": agRaw} {
		for label, secret := range c.allSecrets() {
			if !strings.Contains(raw, secret) {
				continue
			}
			// Old agents log the raw target URL at startup; that predates this
			// work and cannot be fixed in a frozen binary.
			if _, isTarget := targetSecrets[label]; isTarget && name == "agent" && c.as.Old {
				legacyLeaks = append(legacyLeaks, name+" log contains "+label)
				continue
			}
			leaks = append(leaks, name+" log contains "+label)
		}
	}
	sort.Strings(leaks)
	sort.Strings(legacyLeaks)
	c.add("log_hygiene", len(leaks) == 0, "%s", joinOr(leaks, "no key, token, payload, credential or target secret in logs"))
	if len(legacyLeaks) > 0 {
		c.info("log_hygiene_legacy_agent", "%s (pre-existing old-agent startup log)", strings.Join(legacyLeaks, "; "))
	}
}

// checkReconnect restarts the gateway on the same public port and requires the
// agent to re-home and forward again.
func (c *checker) checkReconnect(ctx context.Context, gwp **gatewayProc, logDir string, env []string) {
	old := *gwp
	publicAddr := old.ready.PublicAddr
	old.stop(15 * time.Second)
	restarted := time.Now()
	gw, err := startGateway(old.cmd.Path, filepath.Join(logDir, "gateway-restarted.log"), filepath.Join(logDir, "gateway-restarted.ready"), publicAddr, env)
	if err != nil {
		c.add("reconnect_after_gateway_restart", false, "restart gateway: %v", err)
		return
	}
	*gwp = gw
	c.gw = gw
	if _, err := gw.waitConnected(ctx, c.o.reconnectTimeout); err != nil {
		c.add("reconnect_after_gateway_restart", false, "%v", err)
		return
	}
	took := time.Since(restarted)
	c.facts["reconnect_seconds"] = took.Seconds()
	resp, err := c.forward(ctx, http.MethodPost, "/echo", strings.NewReader(`{"after":"restart"}`), nil)
	if err != nil {
		c.add("reconnect_after_gateway_restart", false, "forward after reconnect: %v", err)
		return
	}
	_, _ = readAll(resp)
	c.add("reconnect_after_gateway_restart", resp.StatusCode == 200, "re-admitted in %s, forward status %d", took.Round(100*time.Millisecond), resp.StatusCode)
}

func joinOr(items []string, ok string) string {
	if len(items) == 0 {
		return ok
	}
	return strings.Join(items, "; ")
}

func dedupe(items []string) []string {
	seen := map[string]int{}
	var order []string
	for _, it := range items {
		if seen[it] == 0 {
			order = append(order, it)
		}
		seen[it]++
	}
	out := make([]string, 0, len(order))
	for _, it := range order {
		if seen[it] > 1 {
			out = append(out, fmt.Sprintf("%s (x%d)", it, seen[it]))
		} else {
			out = append(out, it)
		}
	}
	return out
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func paths(reqs []upstreamRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Path)
	}
	return out
}
