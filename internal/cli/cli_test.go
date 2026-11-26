package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// survives runs one command and turns a crash into a failed assertion. A
// command that cannot open its client or its socket must REFUSE; the guard
// that makes it do so is held by this assertion, not by whatever the code
// after a deleted guard happens to dereference.
func survives(t reporter, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: the command crashed instead of refusing: %v", what, r)
		}
	}()
	fn()
}

type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

type recorder struct{ msgs []string }

func (*recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func TestSurvivesReportsACrashAsAFailure(t *testing.T) {
	var r recorder
	survives(&r, "x", func() { panic("boom") })
	if len(r.msgs) != 1 || !strings.Contains(r.msgs[0], "boom") || !strings.HasPrefix(r.msgs[0], "x: ") {
		t.Fatalf("a crash was not reported as a failure: %q", r.msgs)
	}
	r = recorder{}
	survives(&r, "x", func() {})
	if len(r.msgs) != 0 {
		t.Fatalf("no crash, yet reported: %q", r.msgs)
	}
}

func TestAgentRequiresLabel(t *testing.T) {
	c := AgentCmd()
	var out, errb bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errb)
	c.SetArgs([]string{"--socket", "/nonexistent/s", "verdict", "o/r", strings.Repeat("a", 40)})
	var err error
	survives(t, "no label", func() { err = c.Execute() })
	if err == nil || !strings.Contains(err.Error(), "--label") {
		t.Fatalf("got %v", err)
	}
}

func TestAgentUnreachableDaemonIsStructured(t *testing.T) {
	c := AgentCmd()
	var out, errb bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errb)
	c.SetArgs([]string{"--label", "x", "--socket", "/nonexistent/s", "verdict", "o/r", strings.Repeat("a", 40)})
	var err error
	survives(t, "unreachable", func() { err = c.Execute() })
	ee, ok := err.(exitError)
	if !ok || ee.code != ExitError {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(errb.String(), `"reason":"UNAVAILABLE"`) {
		t.Errorf("stderr %q", errb.String())
	}
}

func TestMainUsesOsExit(t *testing.T) {
	saved := osExit
	defer func() { osExit = saved }()
	got := -1
	osExit = func(c int) { got = c }
	c := AgentCmd()
	c.SetErr(&bytes.Buffer{})
	c.SetArgs([]string{"--label", "x", "--socket", "/nonexistent/s", "verdict", "o/r", strings.Repeat("a", 40)})
	survives(t, "main", func() { Main(c) })
	if got != ExitError {
		t.Errorf("exit %d", got)
	}
}

func TestVerdictExit(t *testing.T) {
	if verdictExit("GREEN", false) != nil {
		t.Error("green")
	}
	if e := verdictExit("RED", false).(exitError); e.code != ExitRed {
		t.Error("red")
	}
	if e := verdictExit("PENDING", false).(exitError); e.code != ExitPending {
		t.Error("pending")
	}
	if e := verdictExit("GREEN", true).(exitError); e.code != ExitPending {
		t.Error("timed out green must not pass")
	}
}
