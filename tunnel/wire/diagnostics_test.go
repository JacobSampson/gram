package wire

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTargetDisplayRemovesCredentialsQueryAndFragment(t *testing.T) {
	require.Equal(t, "https://internal.example:8443/mcp%2Fv2", TargetDisplay("https://user:secret@internal.example:8443/mcp%2Fv2?token=private#secret"))
	require.Empty(t, TargetDisplay("file:///private/key"))
	require.Empty(t, TargetDisplay("https://example.com/"+strings.Repeat("a", MaxTargetDisplayBytes)))
}
func TestOptionalCapabilityIgnoresMalformedHeaders(t *testing.T) {
	require.False(t, SupportsDiagnostics(DiagnosticsCapability, "secret"))
	require.False(t, SupportsDiagnostics("future.v2", strings.Repeat("a", 64)))
	require.True(t, SupportsDiagnostics("future.v2, diagnostics.v1", strings.Repeat("a", 64)))
}
func TestDiagnosticsRejectArbitraryErrorLabels(t *testing.T) {
	step := DiagnosticStep{State: "not_tested"}
	r := DiagnosticsReport{Version: 1, TargetState: "pending", DNS: step, TCP: step, TLS: step}
	require.NoError(t, r.Validate())
	r.LastTransportError = "password=private"
	require.Error(t, r.Validate())
	r.LastTransportError = ""
	r.DNS.State = "private-hostname"
	require.Error(t, r.Validate())
}
