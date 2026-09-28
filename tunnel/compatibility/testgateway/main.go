// Command testgateway runs the tunnel gateway library with in-memory key and
// route stores for black-box compatibility testing.
//
// The same source is compiled against a frozen snapshot of the old tunnel
// packages and against the current tree, so it must only use gateway and route
// APIs that exist in both. Version-specific construction lives behind the
// compatnew build tag.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/speakeasy-api/gram/tunnel/gateway"
	"github.com/speakeasy-api/gram/tunnel/route"
)

type readyInfo struct {
	PublicAddr  string `json:"public_addr"`
	ForwardAddr string `json:"forward_addr"`
	AdminAddr   string `json:"admin_addr"`
	PID         int    `json:"pid"`
}

type stateInfo struct {
	ActiveSessions int                           `json:"active_sessions"`
	Candidates     []string                      `json:"candidates"`
	Connections    map[string][]route.Connection `json:"connections"`
}

func main() {
	publicAddr := flag.String("public", "127.0.0.1:0", "agent /connect listener")
	forwardAddr := flag.String("forward", "127.0.0.1:0", "internal forward listener")
	adminAddr := flag.String("admin", "127.0.0.1:0", "harness state listener")
	readyFile := flag.String("ready-file", "", "write listener addresses as JSON once serving")
	logLevel := flag.String("log-level", "debug", "slog level: debug, info, warn, error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid -log-level: %v\n", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	ctx := context.Background()

	tunnelID := strings.TrimSpace(os.Getenv("COMPAT_TUNNEL_ID"))
	tunnelKey := strings.TrimSpace(os.Getenv("COMPAT_TUNNEL_KEY"))
	forwardToken := strings.TrimSpace(os.Getenv("COMPAT_FORWARD_TOKEN"))
	if tunnelID == "" || tunnelKey == "" || forwardToken == "" {
		logger.ErrorContext(ctx, "COMPAT_TUNNEL_ID, COMPAT_TUNNEL_KEY and COMPAT_FORWARD_TOKEN are required")
		os.Exit(2)
	}

	publicLn, err := net.Listen("tcp", *publicAddr)
	if err != nil {
		logger.ErrorContext(ctx, "listen public", slog.Any("error", err))
		os.Exit(1)
	}
	forwardLn, err := net.Listen("tcp", *forwardAddr)
	if err != nil {
		logger.ErrorContext(ctx, "listen forward", slog.Any("error", err))
		os.Exit(1)
	}
	adminLn, err := net.Listen("tcp", *adminAddr)
	if err != nil {
		logger.ErrorContext(ctx, "listen admin", slog.Any("error", err))
		os.Exit(1)
	}

	keys := gateway.NewStaticKeyStore(map[string]string{tunnelID: tunnelKey})
	routes := newSnapshotStore()
	gw, err := newGateway(forwardToken, forwardLn.Addr().String(), keys, routes, logger)
	if err != nil {
		logger.ErrorContext(ctx, "gateway init", slog.Any("error", err))
		os.Exit(1)
	}

	admin := http.NewServeMux()
	admin.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		candidates, err := routes.Candidates(r.Context(), tunnelID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stateInfo{
			ActiveSessions: gw.ActiveSessions(),
			Candidates:     candidates,
			Connections:    routes.snapshot(tunnelID),
		})
	})

	publicSrv := &http.Server{Handler: gw.PublicHandler(), ReadHeaderTimeout: 15 * time.Second}
	forwardSrv := &http.Server{Handler: gw.ForwardHandler(), ReadHeaderTimeout: 15 * time.Second}
	adminSrv := &http.Server{Handler: admin, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 3)
	for _, s := range []struct {
		srv *http.Server
		ln  net.Listener
	}{{publicSrv, publicLn}, {forwardSrv, forwardLn}, {adminSrv, adminLn}} {
		go func() {
			if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	if *readyFile != "" {
		body, _ := json.Marshal(readyInfo{
			PublicAddr:  publicLn.Addr().String(),
			ForwardAddr: forwardLn.Addr().String(),
			AdminAddr:   adminLn.Addr().String(),
			PID:         os.Getpid(),
		})
		tmp := *readyFile + ".tmp"
		if err := os.WriteFile(tmp, body, 0o600); err != nil {
			logger.ErrorContext(ctx, "write ready file", slog.Any("error", err))
			os.Exit(1)
		}
		if err := os.Rename(tmp, *readyFile); err != nil {
			logger.ErrorContext(ctx, "rename ready file", slog.Any("error", err))
			os.Exit(1)
		}
	}
	logger.InfoContext(ctx, "testgateway listening",
		slog.String("public_addr", publicLn.Addr().String()),
		slog.String("forward_addr", forwardLn.Addr().String()),
		slog.String("admin_addr", adminLn.Addr().String()))

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-sigCtx.Done():
	case err := <-errCh:
		logger.ErrorContext(ctx, "listener failed", slog.Any("error", err))
	}

	// Mirror the production shutdown order: drain routes, stop forwards, close sessions.
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drainCtx, cancelDrain := context.WithTimeout(shutCtx, 3*time.Second)
	gw.Drain(drainCtx)
	cancelDrain()
	_ = forwardSrv.Shutdown(shutCtx)
	closeCtx, cancelClose := context.WithTimeout(shutCtx, 3*time.Second)
	gw.CloseSessions(closeCtx)
	cancelClose()
	_ = publicSrv.Shutdown(shutCtx)
	_ = adminSrv.Shutdown(shutCtx)
	logger.InfoContext(ctx, "testgateway stopped")
}

// snapshotStore keeps route owners and the latest published connection
// snapshots in memory so the harness can inspect what the gateway would have
// projected to Redis. Connections are serialized with route.Connection's own
// JSON tags, so fields added by newer gateways appear without driver changes.
type snapshotStore struct {
	*route.RouteTable

	mu        sync.Mutex
	snapshots map[string]map[string][]route.Connection
}

func newSnapshotStore() *snapshotStore {
	return &snapshotStore{
		RouteTable: route.NewRouteTable(),
		mu:         sync.Mutex{},
		snapshots:  make(map[string]map[string][]route.Connection),
	}
}

func (s *snapshotStore) PublishConnections(_ context.Context, tunnelID, owner string, connections []route.Connection, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshots[tunnelID] == nil {
		s.snapshots[tunnelID] = make(map[string][]route.Connection)
	}
	s.snapshots[tunnelID][owner] = append([]route.Connection(nil), connections...)
	return nil
}

func (s *snapshotStore) Connections(_ context.Context, tunnelID string) ([]route.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []route.Connection
	for _, conns := range s.snapshots[tunnelID] {
		out = append(out, conns...)
	}
	return out, nil
}

func (s *snapshotStore) DeleteConnectionOwner(_ context.Context, tunnelID, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshots[tunnelID], owner)
	if len(s.snapshots[tunnelID]) == 0 {
		delete(s.snapshots, tunnelID)
	}
	return nil
}

func (s *snapshotStore) DeleteConnections(_ context.Context, tunnelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshots, tunnelID)
	return nil
}

func (s *snapshotStore) snapshot(tunnelID string) map[string][]route.Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]route.Connection, len(s.snapshots[tunnelID]))
	for owner, conns := range s.snapshots[tunnelID] {
		out[owner] = append([]route.Connection(nil), conns...)
	}
	return out
}

var _ route.RuntimeStore = (*snapshotStore)(nil)
