// Command harness runs black-box compatibility checks between tunnel agent and
// gateway builds from different source revisions.
//
// It launches real processes (and optionally the published agent image), puts
// an instrumented fake MCP target behind the agent, and drives traffic through
// the gateway's internal forward listener exactly as gram-server does. Every
// assertion is about externally observable behavior, so frozen old binaries
// need no test hooks. run.sh builds the binaries and invokes this program.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

type checkResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Info   bool   `json:"info,omitempty"`
	Detail string `json:"detail"`
}

type pairResult struct {
	Agent    string         `json:"agent"`
	Gateway  string         `json:"gateway"`
	Pass     bool           `json:"pass"`
	Duration string         `json:"duration"`
	LogDir   string         `json:"log_dir"`
	Checks   []checkResult  `json:"checks"`
	Facts    map[string]any `json:"facts"`
}

type report struct {
	StartedAt  time.Time       `json:"started_at"`
	Provenance json.RawMessage `json:"provenance,omitempty"`
	Options    map[string]any  `json:"options"`
	Pairs      []pairResult    `json:"pairs"`
	Pass       bool            `json:"pass"`
}

type gatewaySpec struct {
	Kind string
	Bin  string
	Env  []string
	Old  bool
}

type options struct {
	binDir           string
	outDir           string
	image            string
	mainImage        string
	agents           []string
	gateways         []string
	soak             time.Duration
	tick             time.Duration
	reconnect        bool
	newGatewayEnv    []string
	newAgentEnv      []string
	requireDiag      bool
	provenancePath   string
	dirtyTarget      bool
	faults           []string
	faultDuration    time.Duration
	nearFullStreams  int
	nearFullHold     time.Duration
	transitions      []string
	connectTimeout   time.Duration
	reconnectTimeout time.Duration
}

func main() {
	var o options
	var agents, gateways, newGatewayEnv, newAgentEnv string
	flag.StringVar(&o.binDir, "bin-dir", "", "directory with old/ and new/ tunnel-agent and testgateway binaries")
	flag.StringVar(&o.outDir, "out", "", "output directory for logs and report.json")
	flag.StringVar(&o.image, "image", "", "published 0.1.0 agent image reference for the old-image agent kind")
	flag.StringVar(&o.mainImage, "main-image", "", "published 0.1.1 agent image reference for the main-image agent kind")
	flag.StringVar(&agents, "agents", "old-src,old-image,main-src,main-image,new-src,new-src-nodiag", "agent kinds: old-src, old-image, main-src, main-image, new-src, new-src-nodiag")
	flag.StringVar(&gateways, "gateways", "old-src,main-src,new-src,new-src-nodiag", "gateway kinds: old-src, main-src, new-src, new-src-nodiag")
	flag.DurationVar(&o.soak, "soak", 40*time.Second, "idle soak with one long-lived stream; keep above diagnostic poll interval")
	flag.DurationVar(&o.tick, "tick", 2*time.Second, "event spacing of the long-lived stream during soak")
	flag.BoolVar(&o.reconnect, "reconnect", true, "restart the gateway and require the agent to reconnect")
	flag.StringVar(&newGatewayEnv, "new-gateway-env", "TUNNEL_DIAGNOSTICS_ENABLED=1", "comma-separated KEY=VALUE for new gateways")
	flag.StringVar(&newAgentEnv, "new-agent-env", "", "comma-separated KEY=VALUE for new agents")
	var faults string
	flag.StringVar(&faults, "faults", "noaccept,slowstatus,malformed,notfound", "faultagent modes to run against the new diagnostics-enabled gateway; empty skips")
	flag.DurationVar(&o.faultDuration, "fault-duration", 100*time.Second, "per-fault observation window; keep above yamux's 75s StreamOpenTimeout")
	var transitions string
	flag.StringVar(&transitions, "transitions", "gateway-rollback,gateway-rollback-main,agent-rollback,agent-rollback-main", "rollback transitions: gateway-rollback (new->old-src->new gateway, same agent), gateway-rollback-main (new->main-src->new), agent-rollback (new agent replaced by old-image or old-src), agent-rollback-main (new agent replaced by main-image or main-src); empty skips")
	flag.IntVar(&o.nearFullStreams, "nearfull-streams", 232, "concurrent user streams for the near-full scenario (gateway skips polls at >=224 yamux streams, caps users at 256); 0 skips")
	flag.DurationVar(&o.nearFullHold, "nearfull-hold", 70*time.Second, "how long near-full streams stay open; keep above two poll intervals")
	flag.BoolVar(&o.dirtyTarget, "dirty-target", true, "add userinfo, query and fragment to the pinned target URL and assert they never leak")
	flag.BoolVar(&o.requireDiag, "require-diagnostics", true, "require a completed, reachable diagnostics report and sanitized target display on negotiated new/new pairs")
	flag.StringVar(&o.provenancePath, "provenance", "", "provenance JSON written by run.sh, embedded in the report")
	flag.DurationVar(&o.connectTimeout, "connect-timeout", 30*time.Second, "time allowed for the first agent admission")
	flag.DurationVar(&o.reconnectTimeout, "reconnect-timeout", 45*time.Second, "time allowed for re-admission after gateway restart")
	flag.Parse()

	if o.binDir == "" || o.outDir == "" {
		fmt.Fprintln(os.Stderr, "-bin-dir and -out are required")
		os.Exit(2)
	}
	if o.tick <= 0 || (o.nearFullStreams > 0 && o.nearFullHold < 65*time.Second) {
		fmt.Fprintln(os.Stderr, "-tick must be positive; enabled -nearfull-hold must be at least 65s")
		os.Exit(2)
	}
	o.agents = splitList(agents)
	o.gateways = splitList(gateways)
	o.newGatewayEnv = splitList(newGatewayEnv)
	o.newAgentEnv = splitList(newAgentEnv)
	o.faults = splitList(faults)
	for _, mode := range o.faults {
		// A mistyped list (for example an unsplit "-faults= -transitions=")
		// must stop the run rather than become a failing scenario.
		if !slices.Contains([]string{"noaccept", "slowstatus", "malformed", "notfound"}, mode) {
			fmt.Fprintf(os.Stderr, "unknown fault mode %q\n", mode)
			os.Exit(2)
		}
	}
	o.transitions = splitList(transitions)
	for _, tr := range o.transitions {
		if !slices.Contains([]string{"gateway-rollback", "gateway-rollback-main", "agent-rollback", "agent-rollback-main"}, tr) {
			fmt.Fprintf(os.Stderr, "unknown transition %q\n", tr)
			os.Exit(2)
		}
	}

	// old-* is the 0.1.0 baseline, which joins non-root paths beneath the
	// pinned path. main-* is the pre-feature main line (0.1.1), which keeps
	// OAuth back-channel paths. Both predate diagnostics, so both are Old.
	agentSpecs := map[string]agentSpec{
		"old-src":        {Kind: "old-src", Bin: filepath.Join(o.binDir, "old", "tunnel-agent"), Old: true},
		"old-image":      {Kind: "old-image", Image: o.image, Old: true},
		"main-src":       {Kind: "main-src", Bin: filepath.Join(o.binDir, "main", "tunnel-agent"), Old: true, PreservesPath: true},
		"main-image":     {Kind: "main-image", Image: o.mainImage, Old: true, PreservesPath: true},
		"new-src":        {Kind: "new-src", Bin: filepath.Join(o.binDir, "new", "tunnel-agent"), Env: o.newAgentEnv, Diagnostics: true, PreservesPath: true},
		"new-src-nodiag": {Kind: "new-src-nodiag", Bin: filepath.Join(o.binDir, "new", "tunnel-agent"), Env: overrideEnv(o.newAgentEnv, "TUNNEL_DISABLE_DIAGNOSTICS", "1"), PreservesPath: true},
	}
	gatewaySpecs := map[string]gatewaySpec{
		"old-src":  {Kind: "old-src", Bin: filepath.Join(o.binDir, "old", "testgateway"), Old: true},
		"main-src": {Kind: "main-src", Bin: filepath.Join(o.binDir, "main", "testgateway"), Old: true},
		"new-src":  {Kind: "new-src", Bin: filepath.Join(o.binDir, "new", "testgateway"), Env: o.newGatewayEnv},
		// Same binary with collection switched off; the later assignment wins.
		"new-src-nodiag": {Kind: "new-src-nodiag", Bin: filepath.Join(o.binDir, "new", "testgateway"), Env: overrideEnv(o.newGatewayEnv, "TUNNEL_DIAGNOSTICS_ENABLED", "0")},
	}

	for _, kind := range o.agents {
		if _, ok := agentSpecs[kind]; !ok {
			fmt.Fprintf(os.Stderr, "unknown agent kind %q\n", kind)
			os.Exit(2)
		}
	}
	for _, kind := range o.gateways {
		if _, ok := gatewaySpecs[kind]; !ok {
			fmt.Fprintf(os.Stderr, "unknown gateway kind %q\n", kind)
			os.Exit(2)
		}
	}
	if err := os.MkdirAll(o.outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rep := report{
		StartedAt: time.Now().UTC(),
		Options: map[string]any{
			"agents": o.agents, "gateways": o.gateways, "soak": o.soak.String(), "tick": o.tick.String(),
			"reconnect": o.reconnect, "new_gateway_env": o.newGatewayEnv, "new_agent_env": o.newAgentEnv,
			"require_diagnostics": o.requireDiag, "faults": o.faults, "nearfull_streams": o.nearFullStreams, "nearfull_hold": o.nearFullHold.String(), "transitions": o.transitions, "fault_duration": o.faultDuration.String(), "image": o.image, "main_image": o.mainImage, "dirty_target": o.dirtyTarget,
		},
		Pass: true,
	}
	if o.provenancePath != "" {
		raw, err := os.ReadFile(o.provenancePath)
		var provenance map[string]json.RawMessage
		if err != nil || json.Unmarshal(raw, &provenance) != nil || len(provenance) == 0 {
			fmt.Fprintln(os.Stderr, "provenance must be a readable nonempty JSON object")
			os.Exit(2)
		}
		rep.Provenance = raw
	}

	for _, gk := range o.gateways {
		gs, ok := gatewaySpecs[gk]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown gateway kind %q\n", gk)
			os.Exit(2)
		}
		for _, ak := range o.agents {
			as, ok := agentSpecs[ak]
			if !ok {
				fmt.Fprintf(os.Stderr, "unknown agent kind %q\n", ak)
				os.Exit(2)
			}
			if as.Image == "" && as.Bin == "" || ak == "old-image" && o.image == "" || ak == "main-image" && o.mainImage == "" {
				rep.Pairs = append(rep.Pairs, pairResult{Agent: ak, Gateway: gk, Pass: false, Checks: []checkResult{{
					Name: "setup", Pass: false, Detail: ak + " requested but its image flag is empty",
				}}})
				rep.Pass = false
				continue
			}
			fmt.Fprintf(os.Stderr, "==> agent=%s gateway=%s\n", ak, gk)
			res := runPair(o, as, gs)
			printChecks(res)
			rep.Pairs = append(rep.Pairs, res)
			rep.Pass = rep.Pass && res.Pass
		}
	}

	for _, mode := range o.faults {
		fmt.Fprintf(os.Stderr, "==> fault=%s gateway=new-src\n", mode)
		res := runFault(o, mode, gatewaySpecs["new-src"])
		printChecks(res)
		rep.Pairs = append(rep.Pairs, res)
		rep.Pass = rep.Pass && res.Pass
	}

	for _, tr := range o.transitions {
		fmt.Fprintf(os.Stderr, "==> transition=%s\n", tr)
		var res pairResult
		switch tr {
		case "gateway-rollback":
			res = runGatewayRollback(o, tr, agentSpecs["new-src"], gatewaySpecs["new-src"], gatewaySpecs["old-src"])
		case "gateway-rollback-main":
			res = runGatewayRollback(o, tr, agentSpecs["new-src"], gatewaySpecs["new-src"], gatewaySpecs["main-src"])
		case "agent-rollback":
			oldAgent := agentSpecs["old-src"]
			if o.image != "" {
				oldAgent = agentSpecs["old-image"]
			}
			res = runAgentRollback(o, tr, agentSpecs["new-src"], oldAgent, gatewaySpecs["new-src"])
		case "agent-rollback-main":
			oldAgent := agentSpecs["main-src"]
			if o.mainImage != "" {
				oldAgent = agentSpecs["main-image"]
			}
			res = runAgentRollback(o, tr, agentSpecs["new-src"], oldAgent, gatewaySpecs["new-src"])
		default:
			fmt.Fprintf(os.Stderr, "unknown transition %q\n", tr)
			os.Exit(2)
		}
		printChecks(res)
		rep.Pairs = append(rep.Pairs, res)
		rep.Pass = rep.Pass && res.Pass
	}

	if o.nearFullStreams > 0 {
		fmt.Fprintln(os.Stderr, "==> nearfull agent=new-src gateway=new-src")
		res := runNearFull(o, agentSpecs["new-src"], gatewaySpecs["new-src"])
		printChecks(res)
		rep.Pairs = append(rep.Pairs, res)
		rep.Pass = rep.Pass && res.Pass
	}

	body, _ := json.MarshalIndent(rep, "", "  ")
	reportPath := filepath.Join(o.outDir, "report.json")
	if err := os.WriteFile(reportPath, body, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tGATEWAY\tRESULT\tFAILED CHECKS\tDURATION")
	for _, p := range rep.Pairs {
		var failed []string
		for _, c := range p.Checks {
			if !c.Pass && !c.Info {
				failed = append(failed, c.Name)
			}
		}
		result := "PASS"
		if !p.Pass {
			result = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Agent, p.Gateway, result, strings.Join(failed, ","), p.Duration)
	}
	_ = tw.Flush()
	fmt.Printf("report: %s\n", reportPath)
	if !rep.Pass {
		os.Exit(1)
	}
}

func printChecks(res pairResult) {
	for _, c := range res.Checks {
		mark := "PASS"
		switch {
		case c.Info:
			mark = "INFO"
		case !c.Pass:
			mark = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "    %-4s %-34s %s\n", mark, c.Name, c.Detail)
	}
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pairEnv is the process environment for one pair: the harness's own
// environment minus anything tunnel-related that could leak between runs.
func pairEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "TUNNEL_") || strings.HasPrefix(k, "COMPAT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func runPair(o options, as agentSpec, gs gatewaySpec) pairResult {
	start := time.Now()
	logDir := filepath.Join(o.outDir, fmt.Sprintf("%s__%s", as.Kind, gs.Kind))
	_ = os.RemoveAll(logDir)
	_ = os.MkdirAll(logDir, 0o755)
	res := pairResult{Agent: as.Kind, Gateway: gs.Kind, LogDir: logDir, Facts: map[string]any{}}
	finish := func() pairResult {
		res.Pass = !slices.ContainsFunc(res.Checks, func(c checkResult) bool { return !c.Pass && !c.Info })
		res.Duration = time.Since(start).Round(time.Second).String()
		return res
	}
	add := func(name string, pass bool, format string, args ...any) {
		res.Checks = append(res.Checks, checkResult{Name: name, Pass: pass, Detail: fmt.Sprintf(format, args...)})
	}
	info := func(name, format string, args ...any) {
		res.Checks = append(res.Checks, checkResult{Name: name, Pass: true, Info: true, Detail: fmt.Sprintf(format, args...)})
	}

	secrets := pairSecrets{
		tunnelID:     "compat-tunnel-" + randomHex(4),
		tunnelKey:    "gram_tunnel_" + randomHex(32),
		forwardToken: "compat-forward-" + randomHex(16),
		sentinel:     "COMPAT-SENTINEL-" + randomHex(8),
		upstreamAuth: "Bearer compat-upstream-cred-" + randomHex(8),
	}
	if o.dirtyTarget {
		secrets.target = targetDecoration{
			user:     "compatuser",
			password: "compatpass" + randomHex(6),
			query:    "compat_target_token=" + randomHex(8),
			fragment: "compatfrag" + randomHex(6),
		}
	}
	gwEnv := pairEnv(append([]string{
		"COMPAT_TUNNEL_ID=" + secrets.tunnelID,
		"COMPAT_TUNNEL_KEY=" + secrets.tunnelKey,
		"COMPAT_FORWARD_TOKEN=" + secrets.forwardToken,
		// Surface yamux's own [ERR]/[WARN] lines, including "aborted stream open".
		"TUNNEL_YAMUX_DEBUG=1",
	}, gs.Env...)...)
	agentBaseEnv := pairEnv("TUNNEL_KEY=" + secrets.tunnelKey)

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
	defer func() { gw.stop(10 * time.Second) }()

	agentLog := filepath.Join(logDir, "agent.log")
	ag, err := startAgent(as, agentLog, gw.ready.PublicAddr, up.addr(), secrets.target, agentBaseEnv)
	if err != nil {
		add("setup", false, "start agent: %v", err)
		return finish()
	}
	defer ag.stop(15 * time.Second)

	ctx := context.Background()
	st, err := gw.waitConnected(ctx, o.connectTimeout)
	if err != nil {
		add("admission", false, "%v", err)
		return finish()
	}
	add("admission", true, "1 live session, route published, snapshot projected")

	c := &checker{
		o: o, as: as, gs: gs, up: up, gw: gw, secrets: secrets,
		add: add, info: info, facts: res.Facts,
	}
	c.run(ctx, st)

	// Log checks cover steady state only; the reconnect phase restarts the
	// gateway and intentionally produces disconnect lines.
	c.checkLogs(gwLog, agentLog, ag)

	if o.reconnect {
		c.checkReconnect(ctx, &gw, logDir, gwEnv)
	}
	return finish()
}

type pairSecrets struct {
	tunnelID     string
	tunnelKey    string
	forwardToken string
	sentinel     string
	upstreamAuth string
	target       targetDecoration
}

// overrideEnv gives scenario switches precedence without duplicate environment keys.
func overrideEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			out = append(out, entry)
		}
	}
	return append(out, key+"="+value)
}
