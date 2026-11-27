// Package aerr is the structured error of the local API: a stable reason, never
// prose (agent-vcs.md "Error model").
package aerr

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-vcs/agentd/internal/verdict"
)

// Reasons. They are part of the protocol and never change spelling.
const (
	Malformed          = "MALFORMED"
	AbbreviatedID      = "ABBREVIATED_ID"
	RequestIDReused    = "REQUEST_ID_REUSED"
	BoundExceeded      = "BOUND_EXCEEDED"
	RateLimited        = "RATE_LIMITED"
	RefMoved           = "REF_MOVED"
	VerdictRed         = "VERDICT_RED"
	VerdictPending     = "VERDICT_PENDING"
	Conflict           = "CONFLICT"
	ChangeClosed       = "CHANGE_CLOSED"
	NotPermitted       = "NOT_PERMITTED"
	NotFound           = "NOT_FOUND"
	UnknownSession     = "UNKNOWN_SESSION"
	Unavailable        = "UNAVAILABLE"
	Internal           = "INTERNAL"
	UnsupportedBackend = "UNSUPPORTED_BY_BACKEND"
	ObjectMismatch     = "OBJECT_MISMATCH" // the upstream's bytes do not hash to the id asked for
)

// Error is both the Go error and the JSON body of a refusal.
type Error struct {
	Reason        string                `json:"reason"`
	Retryable     bool                  `json:"retryable"`
	RetryAfterSec int                   `json:"retry_after_sec,omitempty"`
	Message       string                `json:"message,omitempty"`
	CurrentCommit string                `json:"current_commit,omitempty"`
	Commit        string                `json:"commit,omitempty"`
	Checks        []verdict.CheckResult `json:"checks,omitempty"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Message
}

// New returns an error with a reason and a short, secret-free message.
func New(reason, msg string) *Error {
	return &Error{Reason: reason, Message: msg, Retryable: reason == Unavailable || reason == RateLimited || reason == VerdictPending}
}

// RetryAfter sets the delay a client should wait before retrying.
func (e *Error) RetryAfter(d time.Duration) *Error {
	e.RetryAfterSec = int(d.Round(time.Second) / time.Second)
	return e
}

// As returns err as an *Error, wrapping anything else as Internal without
// detail: the cause belongs in the daemon's log, not in an answer.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Reason: Internal}
}

// HTTPStatus is the status line that carries the error on the local socket.
func (e *Error) HTTPStatus() int {
	switch e.Reason {
	case Malformed, AbbreviatedID, RequestIDReused:
		return http.StatusBadRequest
	case BoundExceeded:
		return http.StatusRequestEntityTooLarge
	case RateLimited:
		return http.StatusTooManyRequests
	case RefMoved:
		return http.StatusConflict
	case VerdictRed, VerdictPending, Conflict, ChangeClosed:
		return http.StatusPreconditionFailed
	case NotPermitted:
		return http.StatusForbidden
	case NotFound, UnknownSession:
		return http.StatusNotFound
	case Unavailable:
		return http.StatusServiceUnavailable
	case UnsupportedBackend:
		return http.StatusNotImplemented
	case ObjectMismatch:
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}
