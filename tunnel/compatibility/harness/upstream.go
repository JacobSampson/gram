package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// upstreamBasePath is the path component of the pinned target URL given to the
// agent. Forwarded "/" maps to it in every version. Other paths are joined
// beneath it by 0.1.0 agents and kept as-is from 0.1.1 onward (see
// agentSpec.targetPath), so the target serves both layouts and every check
// asserts the exact path the agent's version must produce.
const upstreamBasePath = "/mcp"

// oauthTokenPath and oauthMetadataPath stand in for an issuer's back-channel
// endpoints, which Gram forwards on their own paths rather than the MCP path.
const (
	oauthTokenPath    = "/oauth2/token"
	oauthMetadataPath = "/.well-known/oauth-authorization-server"
)

type upstreamRequest struct {
	At         time.Time   `json:"at"`
	Method     string      `json:"method"`
	Path       string      `json:"path"`
	RawQuery   string      `json:"raw_query"`
	Header     http.Header `json:"header"`
	BodySHA256 string      `json:"body_sha256"`
	BodyLen    int64       `json:"body_len"`
	RemoteAddr string      `json:"remote_addr"`
}

type upstreamConn struct {
	At         time.Time `json:"at"`
	RemoteAddr string    `json:"remote_addr"`
}

// upstream is the fake MCP target. It records every request and every accepted
// TCP connection so the harness can prove which traffic reached the target.
type upstream struct {
	srv *http.Server
	ln  net.Listener

	mu       sync.Mutex
	requests []upstreamRequest
	conns    []upstreamConn
	releases map[string]chan struct{}
}

func startUpstream() (*upstream, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen upstream: %w", err)
	}
	u := &upstream{ln: ln, releases: make(map[string]chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc(upstreamBasePath, u.handleRPC)
	for _, prefix := range []string{upstreamBasePath, ""} {
		mux.HandleFunc(prefix+"/echo", u.handleEcho)
		mux.HandleFunc(prefix+"/sink", u.handleSink)
		mux.HandleFunc(prefix+"/blob", u.handleBlob)
		mux.HandleFunc(prefix+"/sse", u.handleSSE)
		mux.HandleFunc(prefix+"/ticker", u.handleTicker)
		mux.HandleFunc(prefix+oauthTokenPath, u.handleEcho)
		mux.HandleFunc(prefix+oauthMetadataPath+"/", u.handleEcho)
	}
	u.srv = &http.Server{
		Handler:           u.record(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ConnState: func(c net.Conn, state http.ConnState) {
			if state != http.StateNew {
				return
			}
			u.mu.Lock()
			u.conns = append(u.conns, upstreamConn{At: time.Now(), RemoteAddr: c.RemoteAddr().String()})
			u.mu.Unlock()
		},
	}
	go func() { _ = u.srv.Serve(ln) }()
	return u, nil
}

func (u *upstream) addr() string { return u.ln.Addr().String() }

func (u *upstream) close() {
	u.mu.Lock()
	for id, ch := range u.releases {
		close(ch)
		delete(u.releases, id)
	}
	u.mu.Unlock()
	_ = u.srv.Close()
}

// record hashes the request body while the handler reads it and appends the
// request once the handler returns.
func (u *upstream) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		counter := &countingReader{r: r.Body, h: h}
		r.Body = io.NopCloser(counter)
		entry := upstreamRequest{
			At:         time.Now(),
			Method:     r.Method,
			Path:       r.URL.Path,
			RawQuery:   r.URL.RawQuery,
			Header:     r.Header.Clone(),
			RemoteAddr: r.RemoteAddr,
		}
		next.ServeHTTP(w, r)
		_, _ = io.Copy(io.Discard, counter)
		entry.BodySHA256 = hex.EncodeToString(h.Sum(nil))
		entry.BodyLen = counter.n
		u.mu.Lock()
		u.requests = append(u.requests, entry)
		u.mu.Unlock()
	})
}

type countingReader struct {
	r io.Reader
	h io.Writer
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	_, _ = c.h.Write(p[:n])
	return n, err
}

func (u *upstream) requestsSnapshot() []upstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamRequest(nil), u.requests...)
}

func (u *upstream) connsSnapshot() []upstreamConn {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamConn(nil), u.conns...)
}

func (u *upstream) releaseChan(id string) chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	ch, ok := u.releases[id]
	if !ok {
		ch = make(chan struct{})
		u.releases[id] = ch
	}
	return ch
}

// release unblocks the /sse handler waiting on id.
func (u *upstream) release(id string) {
	ch := u.releaseChan(id)
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.releases[id]; ok {
		close(ch)
		delete(u.releases, id)
	}
}

type echoResponse struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	RawPath    string            `json:"raw_path"`
	RawQuery   string            `json:"raw_query"`
	Headers    map[string]string `json:"headers"`
	BodySHA256 string            `json:"body_sha256"`
	BodyLen    int               `json:"body_len"`
	Body       string            `json:"body"`
}

// handleEcho reflects the request and returns ?status= when present.
func (u *upstream) handleEcho(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	headers := map[string]string{}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-compat-") {
			headers[name] = r.Header.Get(name)
		}
	}
	status := http.StatusOK
	if raw := r.URL.Query().Get("status"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			status = parsed
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Compat-Upstream", "yes")
	w.Header().Add("X-Compat-Multi", "a")
	w.Header().Add("X-Compat-Multi", "b")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(echoResponse{
		Method:     r.Method,
		Path:       r.URL.Path,
		RawPath:    r.URL.EscapedPath(),
		RawQuery:   r.URL.RawQuery,
		Headers:    headers,
		BodySHA256: hex.EncodeToString(sum[:]),
		BodyLen:    len(body),
		Body:       string(body),
	})
}

// handleRPC answers a JSON-RPC request as JSON, or as a single SSE message
// when the client prefers text/event-stream, like a streamable-HTTP MCP server.
func (u *upstream) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result":  map[string]any{"method": req.Method, "path": r.URL.Path},
	})
	w.Header().Set("Mcp-Session-Id", "compat-session")
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") && r.URL.Query().Get("sse") == "1" {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (u *upstream) handleSink(w http.ResponseWriter, r *http.Request) {
	h := sha256.New()
	n, _ := io.Copy(h, r.Body)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sha256": hex.EncodeToString(h.Sum(nil)), "len": n})
}

func (u *upstream) handleBlob(w http.ResponseWriter, r *http.Request) {
	size, err := strconv.Atoi(r.URL.Query().Get("size"))
	if err != nil || size < 0 {
		http.Error(w, "bad size", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, io.LimitReader(newPatternReader(r.URL.Query().Get("seed")), int64(size)))
}

// handleSSE writes one event, flushes, and blocks until the harness releases
// the id. A client can only see the first event before release if every hop
// streams rather than buffers.
func (u *upstream) handleSSE(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	release := u.releaseChan(id)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "event: message\ndata: one\n\n")
	flusher.Flush()
	select {
	case <-release:
	case <-r.Context().Done():
		return
	case <-time.After(30 * time.Second):
		return
	}
	_, _ = io.WriteString(w, "event: message\ndata: two\n\n")
	flusher.Flush()
}

// handleTicker streams count events spaced by interval, for long-lived stream
// continuity across keepalives and diagnostic polls.
func (u *upstream) handleTicker(w http.ResponseWriter, r *http.Request) {
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	interval, err := time.ParseDuration(r.URL.Query().Get("interval"))
	if err != nil || count <= 0 {
		http.Error(w, "bad ticker params", http.StatusBadRequest)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for i := range count {
		if i > 0 {
			select {
			case <-time.After(interval):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = fmt.Fprintf(w, "data: tick-%d\n\n", i)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// patternReader yields a deterministic byte stream derived from seed.
type patternReader struct {
	block []byte
	off   int
	ctr   uint64
	seed  string
}

func newPatternReader(seed string) *patternReader { return &patternReader{seed: seed} }

func (p *patternReader) Read(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		if p.off >= len(p.block) {
			sum := sha256.Sum256([]byte(p.seed + ":" + strconv.FormatUint(p.ctr, 10)))
			p.block = sum[:]
			p.off = 0
			p.ctr++
		}
		c := copy(b[n:], p.block[p.off:])
		p.off += c
		n += c
	}
	return n, nil
}

func patternSHA256(seed string, size int64) string {
	h := sha256.New()
	_, _ = io.Copy(h, io.LimitReader(newPatternReader(seed), size))
	return hex.EncodeToString(h.Sum(nil))
}
