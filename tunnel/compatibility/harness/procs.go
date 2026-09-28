package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// proc is a child process whose combined output is written to a log file.
type proc struct {
	name    string
	cmd     *exec.Cmd
	logPath string
	logFile *os.File
	done    chan struct{}
	waitErr error
	// stopFn overrides SIGTERM for processes such as `docker run`, whose
	// container must be stopped through the daemon.
	stopFn func() error
}

func startProc(name, logPath string, env []string, bin string, args ...string) (*proc, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create %s log: %w", name, err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &proc{name: name, cmd: cmd, logPath: logPath, logFile: f, done: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		_ = f.Close()
		close(p.done)
	}()
	return p, nil
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// stop asks the process to exit, then kills its process group after grace.
func (p *proc) stop(grace time.Duration) {
	if p == nil || p.exited() {
		return
	}
	if p.stopFn != nil {
		_ = p.stopFn()
	} else {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-p.done:
		return
	case <-time.After(grace):
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.done
}

type gatewayReady struct {
	PublicAddr  string `json:"public_addr"`
	ForwardAddr string `json:"forward_addr"`
	AdminAddr   string `json:"admin_addr"`
	PID         int    `json:"pid"`
}

type gatewayProc struct {
	*proc
	ready gatewayReady
}

func startGateway(bin, logPath, readyPath, publicAddr string, env []string) (*gatewayProc, error) {
	_ = os.Remove(readyPath)
	p, err := startProc("gateway", logPath, env, bin,
		"-public", publicAddr, "-ready-file", readyPath, "-log-level", "debug")
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if p.exited() {
			return nil, fmt.Errorf("gateway exited before ready: %v (log %s)", p.waitErr, logPath)
		}
		body, err := os.ReadFile(readyPath)
		if err == nil {
			var ready gatewayReady
			if err := json.Unmarshal(body, &ready); err != nil {
				p.stop(2 * time.Second)
				return nil, fmt.Errorf("parse gateway ready file: %w", err)
			}
			return &gatewayProc{proc: p, ready: ready}, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.stop(2 * time.Second)
	return nil, errors.New("gateway did not become ready within 15s")
}

type gatewayState struct {
	ActiveSessions int                                     `json:"active_sessions"`
	Candidates     []string                                `json:"candidates"`
	Connections    map[string][]map[string]json.RawMessage `json:"connections"`
}

func (s gatewayState) connections() []map[string]json.RawMessage {
	var out []map[string]json.RawMessage
	for _, conns := range s.Connections {
		out = append(out, conns...)
	}
	return out
}

func (g *gatewayProc) state(ctx context.Context) (gatewayState, error) {
	var st gatewayState
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+g.ready.AdminAddr+"/state", nil)
	if err != nil {
		return st, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("state status %d", resp.StatusCode)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// waitConnected polls gateway state until exactly one agent session is live,
// routed, and projected into a connection snapshot.
func (g *gatewayProc) waitConnected(ctx context.Context, timeout time.Duration) (gatewayState, error) {
	deadline := time.Now().Add(timeout)
	var last gatewayState
	var lastErr error
	for time.Now().Before(deadline) {
		st, err := g.state(ctx)
		if err == nil {
			last = st
			if st.ActiveSessions == 1 && len(st.Candidates) == 1 && len(st.connections()) == 1 {
				return st, nil
			}
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return last, fmt.Errorf("agent not connected within %s (last state %+v, last error %v)", timeout, last, lastErr)
}

// agentSpec describes how to launch one agent flavor.
type agentSpec struct {
	// Kind is the matrix name, e.g. old-src.
	Kind string
	// Bin is the native binary; empty when Image is set.
	Bin string
	// Image runs the agent in a container instead of natively.
	Image string
	// Env adds flavor-specific variables such as a diagnostics opt-out.
	Env []string
	// Old marks frozen pre-change agents that must never see diagnostics traffic.
	Old bool
	// Diagnostics is true when this flavor is expected to negotiate diagnostics.
	Diagnostics bool
	// PreservesPath is true from 0.1.1 onward: a non-root forward keeps its own
	// path (tunneled OAuth back-channel calls) instead of being joined beneath
	// the pinned target path, as 0.1.0 does. "/" maps to the pinned path in both.
	PreservesPath bool
}

// targetPath is the path the target must see for a forwarded non-root path.
func (s agentSpec) targetPath(forwarded string) string {
	if s.PreservesPath {
		return forwarded
	}
	return upstreamBasePath + forwarded
}

// routing names the agent's non-root path semantics for reports.
func (s agentSpec) routing() string {
	if s.PreservesPath {
		return "preserve (0.1.1+)"
	}
	return "join (0.1.0)"
}

// startAgent launches the agent. targetDecor carries userinfo and a query
// string that are spliced into the pinned target URL, so the harness can prove
// they never leave the agent through diagnostics, snapshots or logs.
func startAgent(spec agentSpec, logPath, gatewayPublicAddr, upstreamAddr string, targetDecor targetDecoration, baseEnv []string) (*proc, error) {
	_, gwPort, _ := strings.Cut(gatewayPublicAddr, ":")
	_, upPort, _ := strings.Cut(upstreamAddr, ":")
	host := "127.0.0.1"
	if spec.Image != "" && !runningOnLinux() {
		// Docker Desktop forwards host.docker.internal to host loopback, and the
		// agent accepts ws:// for that host.
		host = "host.docker.internal"
	}
	agentEnv := []string{
		"TUNNEL_GATEWAY_URL=ws://" + host + ":" + gwPort + "/connect",
		"TUNNEL_LOCAL_MCP_URL=" + targetDecor.url(host+":"+upPort),
		"TUNNEL_SERVICE_VERSION=compat-" + spec.Kind,
		`TUNNEL_METADATA={"compat":"harness"}`,
	}
	agentEnv = append(agentEnv, spec.Env...)

	if spec.Image == "" {
		return startProc("agent", logPath, append(append([]string(nil), baseEnv...), agentEnv...), spec.Bin)
	}

	// Pass values through the docker CLI environment so the key never appears in argv.
	name := fmt.Sprintf("gram-tunnel-compat-%d", time.Now().UnixNano())
	args := []string{"run", "--rm", "--name", name, "--pull", "never"}
	if runningOnLinux() {
		args = append(args, "--network", "host")
	}
	env := append(append([]string(nil), baseEnv...), agentEnv...)
	for _, kv := range agentEnv {
		k, _, _ := strings.Cut(kv, "=")
		args = append(args, "-e", k)
	}
	args = append(args, "-e", "TUNNEL_KEY", spec.Image)
	p, err := startProc("agent", logPath, env, "docker", args...)
	if err != nil {
		return nil, err
	}
	p.stopFn = func() error {
		err := exec.Command("docker", "stop", "-t", "5", name).Run()
		// A container can stick in "Created" if the daemon stalls; remove it
		// so it never outlives the run.
		_ = exec.Command("docker", "rm", "-f", name).Run()
		return err
	}
	return p, nil
}

func runningOnLinux() bool {
	out, err := exec.Command("uname", "-s").Output()
	return err == nil && strings.TrimSpace(string(out)) == "Linux"
}

// logLine is the subset of slog JSON output the harness inspects.
type logLine struct {
	Raw   string
	Level string
	Msg   string
}

func readLog(path string) ([]logLine, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var lines []logLine
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		text := sc.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		line := logLine{Raw: text}
		var parsed struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(text), &parsed) == nil {
			line.Level = parsed.Level
			line.Msg = parsed.Msg
		} else if strings.Contains(text, "[ERR]") {
			line.Level = "ERROR"
			line.Msg = text
		} else if strings.Contains(text, "[WARN]") {
			line.Level = "WARN"
			line.Msg = text
		}
		lines = append(lines, line)
	}
	return lines, string(raw), sc.Err()
}

func countMsg(lines []logLine, msg string) int {
	n := 0
	for _, l := range lines {
		if l.Msg == msg {
			n++
		}
	}
	return n
}

// targetDecoration adds components to the pinned target URL that must never be
// displayed or reported: credentials, a query string and a fragment.
type targetDecoration struct {
	user     string
	password string
	query    string
	fragment string
}

func (d targetDecoration) enabled() bool { return d.user != "" }

func (d targetDecoration) url(hostPort string) string {
	if !d.enabled() {
		return "http://" + hostPort + upstreamBasePath
	}
	return "http://" + d.user + ":" + d.password + "@" + hostPort + upstreamBasePath + "?" + d.query + "#" + d.fragment
}

// secrets lists the decoration values that must not appear anywhere.
func (d targetDecoration) secrets() map[string]string {
	if !d.enabled() {
		return nil
	}
	return map[string]string{"target password": d.password, "target query": d.query, "target fragment": d.fragment}
}
