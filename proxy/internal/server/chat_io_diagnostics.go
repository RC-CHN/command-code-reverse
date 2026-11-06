package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"reflect"
	"syscall"
)

// Classify only typed causes. Error strings can contain URLs, credentials,
// upstream debug data or response content and must never be inspected/logged.
func classifyChatIOError(err error) (kind, http2Code string) {
	if err == nil {
		return "none", ""
	}
	var dns *net.DNSError
	var network net.Error
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	switch {
	case errors.Is(err, errUpstreamIdle):
		return "idle_timeout", ""
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded", ""
	case errors.Is(err, context.Canceled):
		return "canceled", ""
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof", ""
	case errors.Is(err, io.EOF):
		return "eof", ""
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset", ""
	case errors.Is(err, syscall.ECONNABORTED):
		return "connection_aborted", ""
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused", ""
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe", ""
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		return "connection_closed", ""
	case errors.As(err, &network) && network.Timeout():
		return "network_timeout", ""
	case errors.As(err, &dns):
		return "dns_error", ""
	case errors.As(err, &certificate), errors.As(err, &record):
		return "tls_error", ""
	}
	// net/http bundles HTTP/2 with unexported error types, so errors.As cannot
	// name them without adding another transport dependency. Match only known
	// package/type identities and read their numeric protocol code; never call
	// Error/String or access stream IDs, Cause, DebugData or other text fields.
	// An unfamiliar future Go representation safely falls back to "other".
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		value := reflect.ValueOf(cause)
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				continue
			}
			value = value.Elem()
		}
		if value.Type().PkgPath() != "net/http" {
			continue
		}
		var code reflect.Value
		switch value.Type().Name() {
		case "http2StreamError":
			if value.Kind() != reflect.Struct {
				continue
			}
			kind, code = "http2_stream_error", value.FieldByName("Code")
		case "http2ConnectionError":
			kind, code = "http2_connection_error", value
		case "http2GoAwayError":
			if value.Kind() != reflect.Struct {
				continue
			}
			kind, code = "http2_goaway", value.FieldByName("ErrCode")
		default:
			continue
		}
		if code.IsValid() && code.Kind() == reflect.Uint32 {
			names := [...]string{"NO_ERROR", "PROTOCOL_ERROR", "INTERNAL_ERROR", "FLOW_CONTROL_ERROR", "SETTINGS_TIMEOUT", "STREAM_CLOSED", "FRAME_SIZE_ERROR", "REFUSED_STREAM", "CANCEL", "COMPRESSION_ERROR", "CONNECT_ERROR", "ENHANCE_YOUR_CALM", "INADEQUATE_SECURITY", "HTTP_1_1_REQUIRED"}
			if n := code.Uint(); n < uint64(len(names)) {
				return kind, names[n]
			}
		}
		return kind, "unknown"
	}
	return "other", ""
}
