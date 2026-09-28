// Command faultagent is a deliberately misbehaving tunnel agent that
// advertises diagnostics support and then mishandles the gateway's control
// polls. The harness uses it to prove that diagnostics failures never close
// the shared yamux session or disturb forwarding.
//
// Modes:
//   - noaccept: never accepts yamux streams, so every SYN from the gateway stays
//     unacknowledged. Exercises yamux's StreamOpenTimeout (75s by default),
//     which closes the whole session unless the opener closes its stream.
//   - slowstatus: forwards normally but holds /_tunnel/status far beyond the
//     gateway's poll deadline.
//   - malformed: forwards normally but answers /_tunnel/status with invalid,
//     oversized, or schema-violating bodies containing a sentinel string.
//   - notfound: forwards normally but answers /_tunnel/status with 404, as an
//     agent that dropped the endpoint would.
//
// Header names are spelled out so this driver does not depend on the wire
// package of either tree.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

func main() {
	mode := flag.String("mode", "", "noaccept, slowstatus, malformed or notfound")
	sentinel := flag.String("sentinel", "FAULT-SENTINEL", "string embedded in malformed status bodies")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gatewayURL := os.Getenv("TUNNEL_GATEWAY_URL")
	target, err := url.Parse(os.Getenv("TUNNEL_LOCAL_MCP_URL"))
	if err != nil || gatewayURL == "" {
		logger.ErrorContext(ctx, "TUNNEL_GATEWAY_URL and TUNNEL_LOCAL_MCP_URL are required")
		os.Exit(2)
	}

	token := make([]byte, 32)
	_, _ = rand.Read(token)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+os.Getenv("TUNNEL_KEY"))
	header.Set("X-Gram-Agent-Version", "fault-"+*mode)
	header.Set("X-Gram-Tunnel-Service-Version", "fault-"+*mode)
	header.Set("X-Gram-Tunnel-Agent-Capabilities", "diagnostics.v1")
	header.Set("X-Gram-Tunnel-Control-Token", hex.EncodeToString(token))
	header.Set("X-Gram-Tunnel-Target-Display", "http://"+target.Host+target.Path)

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, _, err := websocket.Dial(dialCtx, gatewayURL, &websocket.DialOptions{HTTPHeader: header})
	cancel()
	if err != nil {
		logger.ErrorContext(ctx, "dial gateway", slog.Any("error", err))
		os.Exit(1)
	}
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.LogOutput = os.Stderr
	session, err := yamux.Server(conn, cfg)
	if err != nil {
		logger.ErrorContext(ctx, "yamux server", slog.Any("error", err))
		os.Exit(1)
	}
	logger.InfoContext(ctx, "fault agent connected", slog.String("mode", *mode))

	var statusPolls atomic.Int64
	switch *mode {
	case "noaccept":
		// Keepalive pings are answered by yamux itself; streams are never accepted.
	case "slowstatus", "malformed", "notfound":
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.FlushInterval = -1
		mux := http.NewServeMux()
		mux.HandleFunc("/_tunnel/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/_tunnel/status" {
				w.WriteHeader(http.StatusOK)
				return
			}
			n := statusPolls.Add(1)
			logger.InfoContext(r.Context(), "fault agent status poll", slog.Int64("n", n))
			if *mode == "notfound" {
				http.NotFound(w, r)
				return
			}
			if *mode == "slowstatus" {
				select {
				case <-r.Context().Done():
				case <-time.After(60 * time.Second):
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch n % 4 {
			case 0:
				_, _ = fmt.Fprintf(w, `{"version":1,"sequence":%d,"target_state":"reachable","extra":%q}`, n, *sentinel)
			case 1:
				_, _ = fmt.Fprintf(w, `{"version":1,"target_state":%q}`, *sentinel)
			case 2:
				_, _ = io.WriteString(w, `{"version":1,"sequence":`+strings.Repeat("9", 16<<10)+`}`)
			default:
				_, _ = fmt.Fprintf(w, `not json %s`, *sentinel)
			}
		})
		mux.HandleFunc("/", proxy.ServeHTTP)
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
		go func() {
			if err := srv.Serve(session); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.WarnContext(ctx, "fault agent serve ended", slog.Any("error", err))
			}
		}()
	default:
		logger.ErrorContext(ctx, "unknown mode", slog.String("mode", *mode))
		os.Exit(2)
	}

	select {
	case <-ctx.Done():
		logger.InfoContext(context.Background(), "fault agent stopping", slog.Int64("status_polls", statusPolls.Load()))
	case <-session.CloseChan():
		logger.ErrorContext(context.Background(), "fault agent session closed by peer", slog.Int64("status_polls", statusPolls.Load()))
	}
	_ = session.Close()
}
