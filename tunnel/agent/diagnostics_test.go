package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProbeDoesNotSendHTTPOrMCPRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	target, err := url.Parse(server.URL + "/mcp?secret=never-report")
	require.NoError(t, err)
	state, dns, tcp, tls := probeTarget(t.Context(), target)
	require.Equal(t, "reachable", state)
	require.Equal(t, "not_applicable", dns.State)
	require.Equal(t, "pass", tcp.State)
	require.Equal(t, "not_applicable", tls.State)
	require.Zero(t, requests.Load())
}
func TestPassiveUnauthorizedIsAResponseNotATransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("PRIVATE_PAYLOAD_SENTINEL"))
	}))
	defer server.Close()
	d := newDiagnostics()
	transport := observedTransport{base: http.DefaultTransport, diagnostics: d}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(req)
	require.NoError(t, err)
	defer response.Body.Close()
	report := d.snapshot()
	require.Equal(t, 401, report.LastHTTPStatus)
	require.Zero(t, report.TransportErrorsTotal)
	require.EqualValues(t, 1, report.RequestsTotal)
	require.Equal(t, "pending", report.TargetState)
}
func TestProbeRejectsUntrustedTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	state, _, tcp, tls := probeTarget(t.Context(), target)
	require.Equal(t, "unreachable", state)
	require.Equal(t, "pass", tcp.State)
	require.Equal(t, "tls_untrusted", tls.Failure)
}

type canceledTransport struct{}

func (canceledTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.Canceled
}
func TestConsumerCancellationDoesNotBlameTargetTransport(t *testing.T) {
	d := newDiagnostics()
	transport := observedTransport{base: canceledTransport{}, diagnostics: d}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/mcp", nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(req)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, d.snapshot().TransportErrorsTotal)
}
