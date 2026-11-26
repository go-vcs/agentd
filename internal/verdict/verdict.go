// Package verdict decides whether one exact commit is green.
//
// The rules of "is this commit green" are fleet tool prwait's, and live ONCE, in
// github.com/go-fleettools/prwait/judge (runs per head SHA, latest run per
// (workflow, event), no conclusion is pending, zero jobs is red). This package
// is the adapter: it keeps agentd's wire shapes and adds what is agentd's own.
//
//   - runs are read per head SHA, never per branch or pull request;
//   - nothing observed at all is pending, never green;
//   - the verdict names the full commit it judged, and an observation about a
//     different commit is refused, never merged in.
package verdict

import (
	"fmt"
	"strings"

	pj "github.com/go-fleettools/prwait/judge"

	"github.com/go-vcs/agentd/internal/oid"
)

// State is the gate verdict.
type State string

// Gate verdicts, as in agent-vcs.md "Verdict".
const (
	Green   State = "GREEN"
	Red     State = "RED"
	Pending State = "PENDING"
)

// JobsUnknown marks a run whose job count was not read.
const JobsUnknown = -1

// Run is one workflow run of the commit (REST actions/runs).
type Run struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	WorkflowID int64  `json:"workflow_id"`
	Event      string `json:"event"`
	Number     int    `json:"run_number"`
	Created    string `json:"created_at"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
	// Jobs is the number of jobs the run created, or JobsUnknown.
	Jobs int `json:"-"`
}

// Check is one check run of the commit (REST commits/{sha}/check-runs).
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
}

// CheckResult is the verdict of one run or check.
type CheckResult struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	State  State  `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Verdict is the gate verdict for one full commit id.
type Verdict struct {
	Commit string        `json:"commit"`
	State  State         `json:"state"`
	Checks []CheckResult `json:"checks"`
}

// Latest keeps the most recent run of each (workflow, event).
func Latest(rs []Run) []Run {
	in := make([]pj.Run, len(rs))
	for i, r := range rs {
		in[i] = toJudge(r)
	}
	latest := pj.LatestRuns(in)
	out := make([]Run, len(latest))
	for i, r := range latest {
		out[i] = fromJudge(r)
	}
	return out
}

func toJudge(r Run) pj.Run {
	j := pj.Run{
		ID: r.ID, Name: r.Name, Status: r.Status, HeadSHA: r.HeadSHA,
		Conclusion: r.Conclusion, WorkflowID: r.WorkflowID, Event: r.Event,
		Number: r.Number, Created: r.Created,
	}
	if r.Jobs != JobsUnknown {
		n := r.Jobs
		j.Jobs = &n
	}
	return j
}

func fromJudge(j pj.Run) Run {
	r := Run{
		ID: j.ID, Name: j.Name, Status: j.Status, HeadSHA: j.HeadSHA,
		Conclusion: j.Conclusion, WorkflowID: j.WorkflowID, Event: j.Event,
		Number: j.Number, Created: j.Created, Jobs: JobsUnknown,
	}
	if j.Jobs != nil {
		r.Jobs = *j.Jobs
	}
	return r
}

// judgeOne judges one lane with the shared rules. A workflow run (checkJobs)
// also counts its jobs.
func judgeOne(name, kind, status, conclusion string, jobs int, checkJobs bool) CheckResult {
	var st pj.State
	var detail string
	if checkJobs {
		st, detail = pj.RunState(toJudge(Run{Status: status, Conclusion: conclusion, Jobs: jobs}))
	} else {
		st, detail = pj.Classify(pj.Entry{Status: strings.ToUpper(status), Conclusion: strings.ToUpper(conclusion)})
	}
	return CheckResult{Name: name, Kind: kind, State: State(st), Detail: detail}
}

// Judge decides the gate verdict of commit from what was observed for it.
//
// An observation whose head_sha is not exactly commit is an error: it is a
// verdict about another commit, and mixing it in would bind the answer to the
// wrong revision.
func Judge(commit string, runs []Run, checks []Check) (Verdict, error) {
	if _, err := oid.Parse(commit); err != nil {
		return Verdict{}, fmt.Errorf("commit %q: %w", commit, err)
	}
	for _, r := range runs {
		if r.HeadSHA != commit {
			return Verdict{}, fmt.Errorf("run %d is about commit %q, not %q", r.ID, r.HeadSHA, commit)
		}
	}
	for _, c := range checks {
		if c.HeadSHA != commit {
			return Verdict{}, fmt.Errorf("check %q is about commit %q, not %q", c.Name, c.HeadSHA, commit)
		}
	}
	v := Verdict{Commit: commit, Checks: []CheckResult{}}
	for _, r := range Latest(runs) {
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		v.Checks = append(v.Checks, judgeOne(name+" ("+r.Event+" run)", "workflow_run", r.Status, r.Conclusion, r.Jobs, true))
	}
	for _, c := range checks {
		name := c.Name
		if name == "" {
			name = "(unnamed)"
		}
		v.Checks = append(v.Checks, judgeOne(name, "check_run", c.Status, c.Conclusion, 0, false))
	}
	var red, pending bool
	for _, c := range v.Checks {
		switch c.State {
		case Red:
			red = true
		case Pending:
			pending = true
		}
	}
	switch {
	case red:
		v.State = Red
	case pending, len(v.Checks) == 0:
		v.State = Pending
	default:
		v.State = Green
	}
	return v, nil
}
