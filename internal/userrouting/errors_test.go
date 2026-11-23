package userrouting

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestErrorStatusPreservesHostStatusAndRetryAfter(t *testing.T) {
	err := &HostCallError{Code: "host_call_failed", Message: `{"error":{"type":"usage_limit_reached"}}`, Status: http.StatusTooManyRequests, RetryAfterSeconds: 30}
	status, retryAfter := ErrorStatus(fmt.Errorf("wrapped: %w", err))
	if status != http.StatusTooManyRequests || retryAfter != 30 {
		t.Fatalf("ErrorStatus() = (%d, %d), want (429, 30)", status, retryAfter)
	}
	if !isCodexQuotaExhausted(err) {
		t.Fatal("host error text no longer matches quota detection")
	}
	if got := err.Error(); got != `host_call_failed: {"error":{"type":"usage_limit_reached"}}` {
		t.Fatalf("Error() = %q", got)
	}
}

func TestErrorStatusDefaultsToBadGateway(t *testing.T) {
	for _, err := range []error{errors.New("plain"), &HostCallError{Code: "x", Message: "y"}, &StatusError{Status: 200}} {
		if status, retryAfter := ErrorStatus(err); status != http.StatusBadGateway || retryAfter != 0 {
			t.Fatalf("ErrorStatus(%v) = (%d, %d), want (502, 0)", err, status, retryAfter)
		}
	}
	if status, _ := ErrorStatus(&StatusError{Status: http.StatusBadRequest}); status != http.StatusBadRequest {
		t.Fatalf("StatusError status = %d, want 400", status)
	}
}
