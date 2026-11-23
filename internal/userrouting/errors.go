package userrouting

import (
	"errors"
	"net/http"
)

// HostCallError is a failed host callback envelope. Error keeps the
// "code: message" text that quota and cooldown detection parse.
type HostCallError struct {
	Code              string
	Message           string
	Status            int
	RetryAfterSeconds int
}

func (e *HostCallError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

func (e *HostCallError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Status
}

// ErrorStatus returns the HTTP status and retry-after hint the plugin should
// report to CPA for err, so upstream 4xx/429 responses are not turned into 502.
func ErrorStatus(err error) (status int, retryAfterSeconds int) {
	var withStatus interface{ StatusCode() int }
	if errors.As(err, &withStatus) {
		status = withStatus.StatusCode()
	}
	var hostErr *HostCallError
	if errors.As(err, &hostErr) && hostErr.RetryAfterSeconds > 0 {
		retryAfterSeconds = hostErr.RetryAfterSeconds
	}
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	return status, retryAfterSeconds
}
