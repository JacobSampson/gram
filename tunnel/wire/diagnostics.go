package wire

import (
	"encoding/hex"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	HeaderCapabilities    = "X-Gram-Tunnel-Agent-Capabilities"
	HeaderControlToken    = "X-Gram-Tunnel-Control-Token"
	HeaderTargetDisplay   = "X-Gram-Tunnel-Target-Display"
	DiagnosticsCapability = "diagnostics.v1"
	ControlStatusPath     = ControlPathPrefix + "status"
	MaxDiagnosticsBytes   = 8 << 10
	MaxTargetDisplayBytes = 2 << 10
	DiagnosticsInterval   = 15 * time.Second
	DiagnosticsFreshness  = 45 * time.Second
)

// DiagnosticStep contains only bounded categories and timing, never resolver or TLS error text.
type DiagnosticStep struct {
	State          string `json:"state"`
	DurationMillis int64  `json:"duration_ms"`
	Failure        string `json:"failure"`
}

// DiagnosticsReport is the allowlisted diagnostics.v1 wire contract. Ages are
// relative to serialization on the agent, avoiding dependence on customer clocks.
type DiagnosticsReport struct {
	Version                     int            `json:"version"`
	Sequence                    uint64         `json:"sequence"`
	SampleAgeMillis             int64          `json:"sample_age_ms"`
	TargetState                 string         `json:"target_state"`
	ConsecutiveFailures         uint32         `json:"consecutive_failures"`
	DNS                         DiagnosticStep `json:"dns"`
	TCP                         DiagnosticStep `json:"tcp"`
	TLS                         DiagnosticStep `json:"tls"`
	RequestsTotal               uint64         `json:"requests_total"`
	TransportErrorsTotal        uint64         `json:"transport_errors_total"`
	LastHTTPStatus              int            `json:"last_http_status"`
	LastHTTPResponseAgeMillis   int64          `json:"last_http_response_age_ms"`
	LastTransportError          string         `json:"last_transport_error"`
	LastTransportErrorAgeMillis int64          `json:"last_transport_error_age_ms"`
}

// Validate rejects unbounded or invented metric dimensions before persistence.
func (r DiagnosticsReport) Validate() error {
	if r.Version != 1 || !oneOf(r.TargetState, "pending", "reachable", "unreachable", "unknown") || !validAge(r.SampleAgeMillis) || !validAge(r.LastHTTPResponseAgeMillis) || !validAge(r.LastTransportErrorAgeMillis) {
		return errors.New("invalid diagnostic report")
	}
	if r.LastHTTPStatus != 0 && (r.LastHTTPStatus < 100 || r.LastHTTPStatus > 599) {
		return errors.New("invalid diagnostic HTTP status")
	}
	if !validFailure(r.LastTransportError) {
		return errors.New("invalid transport failure category")
	}
	for _, step := range []DiagnosticStep{r.DNS, r.TCP, r.TLS} {
		if !oneOf(step.State, "pass", "fail", "not_applicable", "not_tested") || step.DurationMillis < 0 || step.DurationMillis > 5000 || !validFailure(step.Failure) {
			return errors.New("invalid diagnostic step")
		}
	}
	return nil
}

func validAge(age int64) bool { return age >= -1 && age <= int64((24*time.Hour)/time.Millisecond) }

func validFailure(value string) bool {
	return oneOf(value, "", "dns_not_found", "dns_timeout", "dns_error", "tcp_refused", "tcp_timeout", "tls_expired", "tls_untrusted", "tls_name_mismatch", "tls_error", "proxy", "unknown")
}

func oneOf(value string, allowed ...string) bool {
	return slices.Contains(allowed, value)
}

// SupportsDiagnostics ignores malformed optional headers without rejecting the tunnel.
func SupportsDiagnostics(capabilities, token string) bool {
	if len(capabilities) > 256 || len(token) != 64 {
		return false
	}
	if _, err := hex.DecodeString(token); err != nil {
		return false
	}
	for capability := range strings.SplitSeq(capabilities, ",") {
		if strings.TrimSpace(capability) == DiagnosticsCapability {
			return true
		}
	}
	return false
}

// TargetDisplay rebuilds an address without userinfo, query or fragment. The
// hostname, port and escaped path are permitted configuration, not metric labels.
func TargetDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	display := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
	if len(display) > MaxTargetDisplayBytes || strings.ContainsAny(display, "\r\n") {
		return ""
	}
	return display
}
