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

// transitionEnv starts one upstream plus a fresh set of pair secrets, shared
// by every process generation in a transition scenario.
type transitionEnv struct {
	o       options
	logDir  string
	up      *upstream
	secrets pairSecrets
	gwEnv   func(gs gatewaySpec) []string
	res     *pairResult
}

func (t *transitionEnv) add(name string, pass bool, format string, args ...any) {
	t.res.Checks = append(t.res.Checks, checkResult{Name: name, Pass: pass, Detail: fmt.Sprintf(format, args...)})
}

func newTransitionEnv(o options, name string, res *pairResult) (*transitionEnv, error) {
	logDir := filepath.Join(o.outDir, "transition-"+name)
	_ = os.RemoveAll(logDir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	res.LogDir = logDir
	up, err := startUpstream()
	if err != nil {
		return nil, err
	}
	secrets := pairSecrets{
		tunnelID:     "compat-tunnel-" + randomHex(4),
		tunnelKey:    "gram_tunnel_" + randomHex(32),
		forwardToken: "compat-forward-" + randomHex(16),
		sentinel:     "COMPAT-SENTINEL-" + randomHex(8),
		upstreamAuth: "Bearer compat-upstream-cred-" + randomHex(8),
	}
	t := &transitionEnv{o: o, logDir: logDir, up: up, secrets: secrets, res: res}
	t.gwEnv = func(gs gatewaySpec) []string {
		return pairEnv(append([]string{
			"COMPAT_TUNNEL_ID=" + secrets.tunnelID,
			"COMPAT_TUNNEL_KEY=" + secrets.tunnelKey,
			"COMPAT_FORWARD_TOKEN=" + secrets.forwardToken,
			"TUNNEL_YAMUX_DEBUG=1",
		}, gs.Env...)...)
	}
	return t, nil
}

// stage verifies one steady state after a transition: admission, a forwarded
// echo, and the diagnostics state this gateway/agent combination must show.
func (t *transitionEnv) stage(ctx context.Context, label string, gw *gatewayProc, as agentSpec, gs gatewaySpec, timeout time.Duration, since time.Time) {
	st, err := gw.waitConnected(ctx, timeout)
	if err != nil {
		t.add(label+"_admission", false, "%v", err)
		return
	}
	t.add(label+"_admission", true, "admitted %s after transition", time.Since(since).Round(100*time.Millisecond))

	c := &checker{o: t.o, as: as, gs: gs, up: t.up, gw: gw, secrets: t.secrets, add: t.add, info: func(string, string, ...any) {}, facts: t.res.Facts}
	resp, err := c.forward(ctx, http.MethodPost, "/echo?stage="+label, strings.NewReader(`{"stage":"`+label+`"}`), http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		t.add(label+"_forward", false, "%v", err)
		return
	}
	raw, _ := readAll(resp)
	var echo echoResponse
	_ = json.Unmarshal(raw, &echo)
	t.add(label+"_forward", resp.StatusCode == 200 && echo.Body == `{"stage":"`+label+`"}` && echo.Path == as.targetPath("/echo"),
		"status=%d path=%s", resp.StatusCode, echo.Path)

	conn := st.connections()[0]
	var agentVersion string
	_ = json.Unmarshal(conn["agent_version"], &agentVersion)
	var diag struct {
		State  string          `json:"state"`
		Report json.RawMessage `json:"report"`
	}
	hasDiag := len(conn["diagnostics"]) > 0
	_ = json.Unmarshal(conn["diagnostics"], &diag)
	_, hasDisplay := conn["target_display"]
	t.res.Facts[label+"_agent_version"] = agentVersion
	t.res.Facts[label+"_diagnostics"] = json.RawMessage(conn["diagnostics"])

	switch {
	case gs.Old:
		var extra []string
		for k := range conn {
			if !slices.Contains(legacyConnectionKeys, k) {
				extra = append(extra, k)
			}
		}
		t.add(label+"_legacy_projection", len(extra) == 0, "old gateway projected extra keys %v", extra)
	case as.Diagnostics:
		// Wait for the first completed poll on the new session.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) && diag.State != "available" {
			time.Sleep(250 * time.Millisecond)
			if d, _, err := gw.diagnostics(ctx); err == nil {
				diag.State = d.State
			}
		}
		t.add(label+"_diagnostics_available", diag.State == "available", "state=%q", diag.State)
	default:
		t.add(label+"_diagnostics_unsupported", hasDiag && diag.State == "unsupported" && len(diag.Report) == 0 && !hasDisplay,
			"state=%q report=%t display=%t agent_version=%q", diag.State, len(diag.Report) > 0, hasDisplay, agentVersion)
	}
	versionOK := agentVersion != "" && (as.Old == (agentVersion == "0.1.0"))
	t.add(label+"_agent_version", versionOK, "agent_version=%q", agentVersion)
}

// runGatewayRollback keeps one new agent running while the gateway on its
// configured address is replaced new -> frozen old -> new.
func runGatewayRollback(o options, name string, as agentSpec, newGW, oldGW gatewaySpec) pairResult {
	start := time.Now()
	res := pairResult{Agent: as.Kind, Gateway: "transition:new->" + oldGW.Kind + "->new", Facts: map[string]any{}}
	finish := func() pairResult {
		res.Pass = !slices.ContainsFunc(res.Checks, func(c checkResult) bool { return !c.Pass && !c.Info })
		res.Duration = time.Since(start).Round(time.Second).String()
		return res
	}
	t, err := newTransitionEnv(o, name, &res)
	if err != nil {
		res.Checks = append(res.Checks, checkResult{Name: "setup", Detail: err.Error()})
		return finish()
	}
	defer t.up.close()
	ctx := context.Background()

	generations := []gatewaySpec{newGW, oldGW, newGW}
	labels := []string{"new_gateway", "rollback_old_gateway", "rollforward_new_gateway"}
	var gw *gatewayProc
	var ag *proc
	defer func() {
		ag.stop(10 * time.Second)
		if gw != nil {
			gw.stop(10 * time.Second)
		}
	}()
	publicAddr := "127.0.0.1:0"
	for i, gs := range generations {
		if gw != nil {
			gw.stop(15 * time.Second)
		}
		replacedAt := time.Now()
		gw, err = startGateway(gs.Bin, filepath.Join(t.logDir, fmt.Sprintf("gateway-%d-%s.log", i, gs.Kind)), filepath.Join(t.logDir, fmt.Sprintf("gateway-%d.ready", i)), publicAddr, t.gwEnv(gs))
		if err != nil {
			t.add(labels[i]+"_start", false, "%v", err)
			return finish()
		}
		publicAddr = gw.ready.PublicAddr
		res.Facts[labels[i]+"_gateway_bin"] = gs.Bin
		if ag == nil {
			ag, err = startAgent(as, filepath.Join(t.logDir, "agent.log"), gw.ready.PublicAddr, t.up.addr(), targetDecoration{}, pairEnv("TUNNEL_KEY="+t.secrets.tunnelKey))
			if err != nil {
				t.add("agent_start", false, "%v", err)
				return finish()
			}
		}
		t.stage(ctx, labels[i], gw, as, gs, o.reconnectTimeout, replacedAt)
	}
	agLines, _, _ := readLog(filepath.Join(t.logDir, "agent.log"))
	connected := countMsg(agLines, "tunnel agent connected")
	t.add("same_agent_process_readmitted", !ag.exited() && connected == len(generations), "agent connected %d times across %d gateway generations, exited=%t", connected, len(generations), ag.exited())
	return finish()
}

// runAgentRollback keeps one new gateway running while the new agent is
// replaced by a frozen old agent (the published image when available).
func runAgentRollback(o options, name string, newAgent, oldAgent agentSpec, gs gatewaySpec) pairResult {
	start := time.Now()
	res := pairResult{Agent: "transition:" + newAgent.Kind + "->" + oldAgent.Kind, Gateway: gs.Kind, Facts: map[string]any{}}
	finish := func() pairResult {
		res.Pass = !slices.ContainsFunc(res.Checks, func(c checkResult) bool { return !c.Pass && !c.Info })
		res.Duration = time.Since(start).Round(time.Second).String()
		return res
	}
	t, err := newTransitionEnv(o, name, &res)
	if err != nil {
		res.Checks = append(res.Checks, checkResult{Name: "setup", Detail: err.Error()})
		return finish()
	}
	defer t.up.close()
	ctx := context.Background()

	gw, err := startGateway(gs.Bin, filepath.Join(t.logDir, "gateway.log"), filepath.Join(t.logDir, "gateway.ready"), "127.0.0.1:0", t.gwEnv(gs))
	if err != nil {
		t.add("gateway_start", false, "%v", err)
		return finish()
	}
	defer gw.stop(10 * time.Second)

	for i, as := range []agentSpec{newAgent, oldAgent} {
		label := []string{"new_agent", "rollback_old_agent"}[i]
		replacedAt := time.Now()
		ag, err := startAgent(as, filepath.Join(t.logDir, fmt.Sprintf("agent-%d-%s.log", i, as.Kind)), gw.ready.PublicAddr, t.up.addr(), targetDecoration{}, pairEnv("TUNNEL_KEY="+t.secrets.tunnelKey))
		if err != nil {
			t.add(label+"_start", false, "%v", err)
			return finish()
		}
		res.Facts[label+"_kind"] = as.Kind
		t.stage(ctx, label, gw, as, gs, o.connectTimeout, replacedAt)
		ag.stop(15 * time.Second)
		if i == 0 {
			// The replaced session must disappear before the old agent arrives.
			deadline := time.Now().Add(10 * time.Second)
			active := -1
			for time.Now().Before(deadline) {
				if st, err := gw.state(ctx); err == nil {
					active = st.ActiveSessions
					if active == 0 {
						break
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.add("new_agent_session_released", active == 0, "active sessions after stopping new agent: %d", active)
		}
	}
	gwLines, _, _ := readLog(filepath.Join(t.logDir, "gateway.log"))
	var warns, teardown []string
	for _, l := range gwLines {
		if !strings.EqualFold(l.Level, "WARN") && !strings.EqualFold(l.Level, "ERROR") {
			continue
		}
		// yamux reports the peer's WebSocket closing when an agent process is
		// stopped; that is the replacement itself, not a fault.
		if strings.Contains(l.Msg, "yamux: Failed to read header") && strings.Contains(l.Msg, "EOF") {
			teardown = append(teardown, l.Msg)
			continue
		}
		warns = append(warns, l.Msg)
	}
	t.add("gateway_no_warnings", len(warns) == 0, "%s", joinOr(dedupe(warns), "no WARN/ERROR lines beyond agent-stop teardown"))
	if len(teardown) > 0 {
		t.res.Checks = append(t.res.Checks, checkResult{Name: "gateway_teardown_lines", Pass: true, Info: true,
			Detail: fmt.Sprintf("%d yamux EOF lines from stopping replaced agents", len(teardown))})
	}
	return finish()
}
