package aerr

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestStatusAndRetryable(t *testing.T) {
	for r, want := range map[string]int{
		Malformed: 400, AbbreviatedID: 400, RequestIDReused: 400, BoundExceeded: 413, RateLimited: 429, RefMoved: 409,
		VerdictRed: 412, VerdictPending: 412, Conflict: 412, ChangeClosed: 412, NotPermitted: 403, NotFound: 404,
		UnknownSession: 404, Unavailable: 503, UnsupportedBackend: 501, ObjectMismatch: 502, Internal: 500, "SOMETHING_NEW": 500,
	} {
		if got := New(r, "").HTTPStatus(); got != want {
			t.Errorf("%s: %d, want %d", r, got, want)
		}
	}
	for r, want := range map[string]bool{Unavailable: true, RateLimited: true, VerdictPending: true, VerdictRed: false, RefMoved: false, Malformed: false} {
		if New(r, "").Retryable != want {
			t.Errorf("%s retryable != %v", r, want)
		}
	}
}

func TestErrorTextAndAs(t *testing.T) {
	if got := New(Malformed, "").Error(); got != Malformed {
		t.Error(got)
	}
	if got := New(Malformed, "why").Error(); got != "MALFORMED: why" {
		t.Error(got)
	}
	wrapped := fmt.Errorf("ctx: %w", New(RefMoved, "m"))
	if As(wrapped).Reason != RefMoved {
		t.Error("wrapped error lost")
	}
	if e := As(errors.New("secret detail")); e.Reason != Internal || e.Message != "" {
		t.Errorf("foreign error leaked: %+v", e)
	}
	if got := New(RateLimited, "").RetryAfter(1500 * time.Millisecond).RetryAfterSec; got != 2 {
		t.Errorf("retry after %d", got)
	}
}
