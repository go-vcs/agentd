package cli

import (
	"strings"
	"testing"
)

// The stream prints each change once, hands the cursor back, skips a quiet
// timeout without printing it, and ends on the final verdict with its exit code.
func TestEventsPrintsEachChangeAndEndsOnTheFinal(t *testing.T) {
	sock := fakeDaemon(t, "ok")
	out, errb, err := runAgent(t, "--label", "x", "--socket", sock, "events", "o/r", id40)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code(err) != ExitRed || errb != "" || len(lines) != 2 ||
		!strings.Contains(lines[0], `"PENDING"`) || strings.Contains(lines[0], "final") ||
		!strings.Contains(lines[1], `"RED"`) || !strings.Contains(lines[1], `"final":true`) {
		t.Fatalf("%v out %q err %q", err, out, errb)
	}
}

func TestEventsReportsARefusalNeedsTwoArgumentsAndAnOverallTimeout(t *testing.T) {
	sock := fakeDaemon(t, "refuse")
	out, errb, err := runAgent(t, "--label", "x", "--socket", sock, "events", "o/r", id40)
	if code(err) != ExitError || out != "" || !strings.Contains(errb, `"reason":"OBJECT_MISMATCH"`) {
		t.Fatalf("%v out %q err %q", err, out, errb)
	}
	if _, _, err := runAgent(t, "--label", "x", "--socket", sock, "events", "o/r"); err == nil {
		t.Fatal("one argument accepted")
	}
	// A stream whose whole timeout has passed ends pending, not as an error.
	_, errb, err = runAgent(t, "--label", "x", "--socket", fakeDaemon(t, "ok"), "events", "o/r", id40, "--timeout", "1ns")
	if code(err) != ExitPending || errb != "" {
		t.Fatalf("expired: %v %q", err, errb)
	}
}
