package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestAgentRequiresLabel(t *testing.T) {
	c := AgentCmd()
	var out, errb bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errb)
	c.SetArgs([]string{"--socket", "/nonexistent/s", "verdict", "o/r", strings.Repeat("a", 40)})
	err := c.Execute()
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
	err := c.Execute()
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
	Main(c)
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
