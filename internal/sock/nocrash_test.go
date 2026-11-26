package sock

import (
	"fmt"
	"strings"
	"testing"
)

// noCrash is deferred by a test whose subject must REFUSE: a refusal path that
// crashes instead is a failed assertion, so the guard that makes the code
// refuse is held by the test and not by whatever a deleted guard dereferences.
func noCrash(t *testing.T) {
	t.Helper()
	reportCrash(t, recover())
}

type failer interface {
	Helper()
	Errorf(format string, args ...any)
}

type recorded struct{ msgs []string }

func (*recorded) Helper() {}
func (r *recorded) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func reportCrash(t failer, r any) {
	t.Helper()
	if r != nil {
		t.Errorf("a refusal path crashed instead of refusing: %v", r)
	}
}

func TestReportCrash(t *testing.T) {
	var r recorded
	reportCrash(&r, nil)
	if len(r.msgs) != 0 {
		t.Fatalf("no crash, yet reported: %q", r.msgs)
	}
	reportCrash(&r, "boom")
	if len(r.msgs) != 1 || !strings.Contains(r.msgs[0], "boom") {
		t.Fatalf("a crash was not reported: %q", r.msgs)
	}
}
