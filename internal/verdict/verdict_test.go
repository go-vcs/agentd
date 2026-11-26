package verdict

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-vcs/agentd/internal/oid"
)

var sha = strings.Repeat("ab", 20)

func run(id int64, wf int64, event string, n int, status, concl string, jobs int) Run {
	return Run{ID: id, Name: "w" + string(rune('0'+wf)), WorkflowID: wf, Event: event, Number: n,
		Created: "2026-11-26T10:00:0" + string(rune('0'+n%10)) + "Z", Status: status, Conclusion: concl, HeadSHA: sha, Jobs: jobs}
}

func chk(name, status, concl string) Check {
	return Check{Name: name, Status: status, Conclusion: concl, HeadSHA: sha}
}

func TestJudge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runs   []Run
		checks []Check
		want   State
	}{
		{"nothing observed is pending", nil, nil, Pending},
		{"all green", []Run{run(1, 1, "pull_request", 1, "completed", "success", 3)}, []Check{chk("test", "completed", "success")}, Green},
		// Trap 1: a red `test` lane beside green ones, as a run that "exits 0".
		{"red lane among green", []Run{run(1, 1, "pull_request", 1, "completed", "failure", 3)}, []Check{chk("a", "completed", "success"), chk("test (macos)", "completed", "failure")}, Red},
		// Trap 2: 24 checks still at conclusion null.
		{"conclusion null is pending", nil, []Check{chk("a", "completed", "success"), chk("b", "in_progress", ""), chk("c", "queued", "")}, Pending},
		{"completed with null conclusion is pending", nil, []Check{chk("a", "completed", "")}, Pending},
		{"in progress run is pending", []Run{run(1, 1, "push", 1, "in_progress", "", JobsUnknown)}, nil, Pending},
		// Trap 3: a run that failed before creating any job leaves nothing red.
		{"zero jobs success is red", []Run{run(1, 1, "pull_request", 1, "completed", "success", 0)}, []Check{chk("test", "completed", "success")}, Red},
		{"zero jobs failure is red", []Run{run(1, 1, "pull_request", 1, "completed", "failure", 0)}, nil, Red},
		{"zero jobs skipped is red", []Run{run(1, 1, "pull_request", 1, "completed", "skipped", 0)}, nil, Red},
		{"job count not read is pending", []Run{run(1, 1, "pull_request", 1, "completed", "success", JobsUnknown)}, nil, Pending},
		{"cancelled is red", nil, []Check{chk("a", "completed", "cancelled")}, Red},
		{"timed_out is red", nil, []Check{chk("a", "completed", "timed_out")}, Red},
		{"neutral and skipped pass", nil, []Check{chk("a", "completed", "neutral"), chk("b", "completed", "skipped")}, Green},
		{"red wins over pending", nil, []Check{chk("a", "in_progress", ""), chk("b", "completed", "failure")}, Red},
		{"retrigger replaces red", []Run{run(1, 1, "pull_request", 1, "completed", "failure", 3), run(2, 1, "pull_request", 2, "completed", "success", 3)}, nil, Green},
		{"dispatch does not mask red pull_request", []Run{run(1, 1, "pull_request", 1, "completed", "failure", 3), run(2, 1, "workflow_dispatch", 2, "completed", "success", 3)}, nil, Red},
		{"newer red replaces older green", []Run{run(1, 1, "push", 1, "completed", "success", 3), run(2, 1, "push", 2, "completed", "failure", 3)}, nil, Red},
	} {
		v, err := Judge(sha, tc.runs, tc.checks)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if v.State != tc.want {
			t.Errorf("%s: state = %s, want %s (%+v)", tc.name, v.State, tc.want, v.Checks)
		}
		if v.Commit != sha {
			t.Errorf("%s: verdict names %q, not the commit judged", tc.name, v.Commit)
		}
	}
}

// Trap 4: a full id reconstructed from a 7-character prefix is refused, and a
// verdict is never given for one commit from observations of another.
func TestJudgeRefusesWrongCommit(t *testing.T) {
	if _, err := Judge(sha[:7], nil, nil); !errors.Is(err, oid.ErrAbbreviated) {
		t.Errorf("prefix: err = %v, want ErrAbbreviated", err)
	}
	if _, err := Judge(sha[:7]+strings.Repeat("0", 33), []Run{run(1, 1, "push", 1, "completed", "success", 2)}, nil); err == nil {
		t.Error("a run about another commit was accepted: a prefix expanded into a fake SHA")
	}
	other := chk("a", "completed", "success")
	other.HeadSHA = strings.Repeat("cd", 20)
	if _, err := Judge(sha, nil, []Check{other}); err == nil {
		t.Error("a check about another commit was accepted")
	}
}

func TestLatestDeterministic(t *testing.T) {
	rs := []Run{run(3, 2, "push", 1, "completed", "success", 1), run(1, 1, "push", 1, "completed", "success", 1), run(2, 1, "push", 1, "completed", "success", 1)}
	rs[2].Created = "2026-11-26T10:00:09Z" // same number: the later creation wins
	got := Latest(rs)
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Errorf("Latest = %+v", got)
	}
}
