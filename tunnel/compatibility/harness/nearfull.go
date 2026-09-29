package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type diagTimes struct {
	State       string    `json:"state"`
	AttemptedAt time.Time `json:"attempted_at"`
	ReceivedAt  time.Time `json:"received_at"`
}

func (g *gatewayProc) diagnostics(ctx context.Context) (diagTimes, int, error) {
	var d diagTimes
	st, err := g.state(ctx)
	if err != nil {
		return d, 0, err
	}
	conns := st.connections()
	if len(conns) != 1 {
		return d, 0, fmt.Errorf("%d connections", len(conns))
	}
	var active int
	_ = json.Unmarshal(conns[0]["active_substreams"], &active)
	err = json.Unmarshal(conns[0]["diagnostics"], &d)
	return d, active, err
}

// runNearFull holds o.nearFullStreams long-lived user streams on one new/new
// session, above the gateway's diagnostics skip threshold, and verifies that
// polls pause without affecting user traffic and resume once pressure drops.
func runNearFull(o options, as agentSpec, gs gatewaySpec) pairResult {
	start := time.Now()
	logDir := filepath.Join(o.outDir, "nearfull__"+as.Kind+"__"+gs.Kind)
	_ = os.RemoveAll(logDir)
	_ = os.MkdirAll(logDir, 0o755)
	res := pairResult{Agent: "nearfull-" + as.Kind, Gateway: gs.Kind, LogDir: logDir, Facts: map[string]any{}}
	add := func(name string, pass bool, format string, args ...any) {
		res.Checks = append(res.Checks, checkResult{Name: name, Pass: pass, Detail: fmt.Sprintf(format, args...)})
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
	ag, err := startAgent(as, agentLog, gw.ready.PublicAddr, up.addr(), targetDecoration{}, pairEnv("TUNNEL_KEY="+secrets.tunnelKey))
	if err != nil {
		add("setup", false, "start agent: %v", err)
		return finish()
	}
	defer ag.stop(10 * time.Second)

	ctx := context.Background()
	if _, err := gw.waitConnected(ctx, o.connectTimeout); err != nil {
		add("admission", false, "%v", err)
		return finish()
	}
	c := &checker{o: o, as: as, gs: gs, gw: gw, up: up, secrets: secrets, add: add, facts: res.Facts}

	// Wait for the first successful poll so a later pause is measurable.
	var before diagTimes
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if d, _, err := gw.diagnostics(ctx); err == nil && d.State == "available" {
			before = d
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	add("nearfull_baseline_available", before.State == "available", "diagnostics before pressure: %+v", before)

	n := o.nearFullStreams
	ticks := int(o.nearFullHold/o.tick) + 1
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Go(func() {
			resp, err := c.forward(ctx, http.MethodGet, fmt.Sprintf("/ticker?count=%d&interval=%s&i=%d", ticks, o.tick, i), nil, http.Header{"Accept": {"text/event-stream"}})
			if err != nil {
				errs <- err.Error()
				return
			}
			defer resp.Body.Close()
			got := 0
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "data: tick-") {
					got++
				}
			}
			if resp.StatusCode != http.StatusOK || got != ticks {
				errs <- fmt.Sprintf("stream %d: status %d ticks %d/%d %s", i, resp.StatusCode, got, ticks, resp.Header.Get(hdrTunnelError))
			}
		})
	}

	// Pressure is established once the gateway counts every stream.
	var pressureAt time.Time
	peak := 0
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, active, err := gw.diagnostics(ctx); err == nil {
			peak = max(peak, active)
			if active >= n {
				pressureAt = time.Now()
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	res.Facts["peak_active_substreams"] = peak
	if pressureAt.IsZero() {
		add("nearfull_pressure_established", false, "only %d/%d streams active", peak, n)
		wg.Wait()
		return finish()
	}
	add("nearfull_pressure_established", true, "%d concurrent user streams active", peak)

	// Short forwards must still work beside the held streams.
	var extraProblems []string
	for i := range 5 {
		resp, err := c.forward(ctx, http.MethodPost, "/echo", strings.NewReader(fmt.Sprintf(`{"extra":%d}`, i)), nil)
		if err != nil {
			extraProblems = append(extraProblems, err.Error())
			continue
		}
		_, _ = readAll(resp)
		if resp.StatusCode != http.StatusOK {
			extraProblems = append(extraProblems, fmt.Sprintf("status %d %s", resp.StatusCode, resp.Header.Get(hdrTunnelError)))
		}
	}
	add("nearfull_extra_forwards", len(extraProblems) == 0, "%s", joinOr(extraProblems, "5 short forwards ok under pressure"))

	// Any poll that started after pressure began would move attempted_at.
	var during diagTimes
	var pollsDuring []time.Time
	for time.Now().Before(pressureAt.Add(o.nearFullHold - 5*time.Second)) {
		if d, active, err := gw.diagnostics(ctx); err == nil {
			if active < n {
				break
			}
			during = d
			if d.AttemptedAt.After(pressureAt) && !slices.Contains(pollsDuring, d.AttemptedAt) {
				pollsDuring = append(pollsDuring, d.AttemptedAt)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	res.Facts["diagnostics_during_pressure"] = during
	add("nearfull_polls_paused", len(pollsDuring) == 0 && time.Since(pressureAt) >= 32*time.Second, "polls started under pressure: %v (window %s)", pollsDuring, time.Since(pressureAt).Round(time.Second))

	wg.Wait()
	close(errs)
	var streamProblems []string
	for e := range errs {
		streamProblems = append(streamProblems, e)
	}
	add("nearfull_streams_complete", len(streamProblems) == 0, "%s", joinOr(dedupe(streamProblems), fmt.Sprintf("%d streams delivered %d ticks each", n, ticks)))

	released := time.Now()
	var after diagTimes
	deadline = released.Add(wireIntervalMax)
	for time.Now().Before(deadline) {
		if d, _, err := gw.diagnostics(ctx); err == nil && d.State == "available" && d.AttemptedAt.After(released) {
			after = d
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	add("nearfull_polls_resume", !after.AttemptedAt.IsZero(), "first poll after release: %s after, state %q", after.AttemptedAt.Sub(released).Round(100*time.Millisecond), after.State)

	gwLines, _, _ := readLog(gwLog)
	agLines, _, _ := readLog(agentLog)
	disconnects := countMsg(gwLines, "tunnel disconnected") + countMsg(agLines, "tunnel agent session ended")
	var warns []string
	for _, l := range append(gwLines, agLines...) {
		if strings.EqualFold(l.Level, "WARN") || strings.EqualFold(l.Level, "ERROR") {
			warns = append(warns, l.Msg)
		}
	}
	add("nearfull_no_teardown_or_warnings", disconnects == 0 && len(warns) == 0 && !ag.exited(), "disconnects=%d warnings=%v", disconnects, dedupe(warns))
	return finish()
}

// wireIntervalMax bounds how long polls may take to resume: one 30s interval
// plus jitter, with margin.
const wireIntervalMax = 40 * time.Second
