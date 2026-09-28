package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// runFault pairs a diagnostics-enabled gateway with faultagent in one mode and
// asserts that diagnostics failures never cost the session or user traffic.
func runFault(o options, mode string, gs gatewaySpec) pairResult {
	start := time.Now()
	logDir := filepath.Join(o.outDir, "fault-"+mode+"__"+gs.Kind)
	_ = os.RemoveAll(logDir)
	_ = os.MkdirAll(logDir, 0o755)
	res := pairResult{Agent: "fault-" + mode, Gateway: gs.Kind, LogDir: logDir, Facts: map[string]any{}}
	add := func(name string, pass bool, format string, args ...any) {
		res.Checks = append(res.Checks, checkResult{Name: name, Pass: pass, Detail: fmt.Sprintf(format, args...)})
	}
	info := func(name, format string, args ...any) {
		res.Checks = append(res.Checks, checkResult{Name: name, Pass: true, Info: true, Detail: fmt.Sprintf(format, args...)})
	}
	finish := func() pairResult {
		res.Pass = !slices.ContainsFunc(res.Checks, func(c checkResult) bool { return !c.Pass && !c.Info })
		res.Duration = time.Since(start).Round(time.Second).String()
		return res
	}

	secrets := pairSecrets{
		tunnelID:     "compat-tunnel-" + randomHex(4),
		tunnelKey:    "gram_tunnel_" + randomHex(32),
		forwardToken: "compat-forward-" + randomHex(16),
		sentinel:     "FAULT-SENTINEL-" + randomHex(8),
	}
	gwEnv := pairEnv(append([]string{
		"COMPAT_TUNNEL_ID=" + secrets.tunnelID,
		"COMPAT_TUNNEL_KEY=" + secrets.tunnelKey,
		"COMPAT_FORWARD_TOKEN=" + secrets.forwardToken,
		"TUNNEL_YAMUX_DEBUG=1",
	}, gs.Env...)...)

	up, err := startUpstream()
	if err != nil {
		add("setup", false, "%v", err)
		return finish()
	}
	defer up.close()
	gwLog := filepath.Join(logDir, "gateway.log")
	gw, err := startGateway(gs.Bin, gwLog, filepath.Join(logDir, "gateway.ready"), "127.0.0.1:0", gwEnv)
	if err != nil {
		add("setup", false, "start gateway: %v", err)
		return finish()
	}
	defer gw.stop(10 * time.Second)

	agentLog := filepath.Join(logDir, "agent.log")
	_, gwPort, _ := strings.Cut(gw.ready.PublicAddr, ":")
	ag, err := startProc("agent", agentLog, pairEnv(
		"TUNNEL_KEY="+secrets.tunnelKey,
		"TUNNEL_GATEWAY_URL=ws://127.0.0.1:"+gwPort+"/connect",
		"TUNNEL_LOCAL_MCP_URL=http://"+up.addr()+upstreamBasePath,
	), filepath.Join(o.binDir, "new", "faultagent"), "-mode", mode, "-sentinel", secrets.sentinel)
	if err != nil {
		add("setup", false, "start fault agent: %v", err)
		return finish()
	}
	defer ag.stop(10 * time.Second)

	ctx := context.Background()
	if _, err := gw.waitConnected(ctx, o.connectTimeout); err != nil {
		add("admission", false, "%v", err)
		return finish()
	}
	add("admission", true, "fault agent admitted with diagnostics capability")

	c := &checker{o: o, gw: gw, secrets: secrets, add: add, info: info, facts: res.Facts, up: up}
	duration := o.faultDuration
	deadline := time.Now().Add(duration)
	var stateProblems, forwardProblems []string
	var maxLatency time.Duration
	forwards := 0
	for time.Now().Before(deadline) {
		st, err := gw.state(ctx)
		switch {
		case err != nil:
			stateProblems = append(stateProblems, "state error: "+err.Error())
		case st.ActiveSessions != 1 || len(st.Candidates) != 1:
			stateProblems = append(stateProblems, fmt.Sprintf("active=%d candidates=%d", st.ActiveSessions, len(st.Candidates)))
		}
		if mode != "noaccept" {
			began := time.Now()
			reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			resp, err := c.forward(reqCtx, http.MethodPost, "/echo", strings.NewReader(`{"fault":"`+mode+`"}`), nil)
			if err != nil {
				forwardProblems = append(forwardProblems, err.Error())
			} else {
				_, _ = readAll(resp)
				if resp.StatusCode != http.StatusOK {
					forwardProblems = append(forwardProblems, fmt.Sprintf("status %d %s", resp.StatusCode, resp.Header.Get(hdrTunnelError)))
				}
			}
			cancel()
			forwards++
			maxLatency = max(maxLatency, time.Since(began))
		}
		time.Sleep(time.Second)
	}
	add("fault_session_stable", len(stateProblems) == 0, "%s", joinOr(dedupe(stateProblems), fmt.Sprintf("1 live session and route at every sample for %s", duration)))
	if mode != "noaccept" {
		ok := len(forwardProblems) == 0 && maxLatency < 2*time.Second
		add("fault_forwarding_unaffected", ok, "%s", joinOr(dedupe(forwardProblems), fmt.Sprintf("%d forwards ok, max latency %s", forwards, maxLatency.Round(time.Millisecond))))
	}

	st, err := gw.state(ctx)
	if err == nil && len(st.connections()) == 1 {
		conn := st.connections()[0]
		whole, _ := json.Marshal(conn)
		add("fault_snapshot_no_sentinel", !strings.Contains(string(whole), secrets.sentinel), "agent-supplied garbage must not be projected")
		var diag struct {
			State  string          `json:"state"`
			Report json.RawMessage `json:"report"`
		}
		_ = json.Unmarshal(conn["diagnostics"], &diag)
		res.Facts["diagnostics"] = json.RawMessage(conn["diagnostics"])
		if mode == "notfound" {
			add("fault_diagnostics_unsupported", diag.State == "unsupported", "state=%q; a 404 status must mark the session unsupported", diag.State)
		} else {
			add("fault_diagnostics_not_available", diag.State != "available", "state=%q (a failing control path must not look healthy)", diag.State)
		}
	} else {
		add("fault_snapshot_no_sentinel", false, "state: err=%v", err)
	}

	gwLines, gwRaw, _ := readLog(gwLog)
	agLines, _, _ := readLog(agentLog)
	var yamuxErrs []string
	for _, l := range gwLines {
		if strings.Contains(l.Raw, "yamux") && (strings.EqualFold(l.Level, "ERROR") || strings.EqualFold(l.Level, "WARN")) {
			yamuxErrs = append(yamuxErrs, l.Msg)
		}
	}
	disconnected := countMsg(gwLines, "tunnel disconnected") + countMsg(agLines, "fault agent session closed by peer")
	add("fault_no_session_teardown", disconnected == 0 && len(yamuxErrs) == 0 && !ag.exited(),
		"disconnect lines=%d yamux errors=%v agent exited=%t", disconnected, dedupe(yamuxErrs), ag.exited())
	add("fault_gateway_log_no_sentinel", !strings.Contains(gwRaw, secrets.sentinel), "rejected status bodies must not be logged")
	polls := countMsg(agLines, "fault agent status poll")
	res.Facts["status_polls"] = polls
	if mode == "notfound" {
		add("fault_polling_stopped", polls == 1, "%d status polls reached the agent in %s; want exactly 1 after a 404", polls, duration)
	} else if mode != "noaccept" {
		info("fault_status_polls", "%d status polls reached the agent in %s", polls, duration)
	}
	var warns []string
	for _, l := range gwLines {
		if strings.EqualFold(l.Level, "WARN") || strings.EqualFold(l.Level, "ERROR") {
			warns = append(warns, l.Msg)
		}
	}
	info("fault_gateway_warnings", "%s", joinOr(dedupe(warns), "none"))
	return finish()
}
