//go:build !compatnew

package main

import (
	"log/slog"

	"github.com/speakeasy-api/gram/tunnel/gateway"
	"github.com/speakeasy-api/gram/tunnel/route"
)

// newGateway constructs the gateway with the API frozen at the old baseline.
// Only use Config fields and constructor arguments that exist there.
func newGateway(forwardToken, advertiseAddr string, keys gateway.KeyResolver, routes route.Store, logger *slog.Logger) (*gateway.Gateway, error) {
	return gateway.New(gateway.Config{
		AdvertiseAddr: advertiseAddr,
		ForwardToken:  forwardToken,
	}, keys, routes, logger)
}
