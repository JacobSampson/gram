//go:build compatnew

package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/speakeasy-api/gram/tunnel/gateway"
	"github.com/speakeasy-api/gram/tunnel/route"
)

// newGateway constructs the gateway from the current tree with diagnostics
// polling enabled. TUNNEL_DIAGNOSTICS_ENABLED=0 or false turns polling off so
// the harness can also cover a new gateway with collection disabled.
func newGateway(forwardToken, advertiseAddr string, keys gateway.KeyResolver, routes route.Store, logger *slog.Logger) (*gateway.Gateway, error) {
	diagnostics := true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TUNNEL_DIAGNOSTICS_ENABLED"))) {
	case "0", "false":
		diagnostics = false
	}
	return gateway.New(gateway.Config{
		AdvertiseAddr:      advertiseAddr,
		ForwardToken:       forwardToken,
		DiagnosticsEnabled: diagnostics,
	}, keys, routes, logger)
}
