// Package verdict decides whether one exact commit is green.
//
// It reproduces the rules fleet tool prwait (go-fleettools/prwait, package
// main, so not importable) learned the hard way, as pure functions of recorded
// observations:
//
//   - runs are read per head SHA, never per branch or pull request;
//   - only the LATEST run per (workflow, event) is judged: a retrigger replaces
//     a red run, and a manual dispatch never masks a red pull_request run;
//   - a check or run with no conclusion yet is pending, never a pass;
//   - a run that finished with ZERO jobs never started: it is red, never green,
//     even when GitHub says "success" (prwait does not count jobs; this does);
//   - nothing observed at all is pending, never green;
//   - the verdict names the full commit it judged, and an observation about a
//     different commit is refused, never merged in.
package verdict

import (
	"fmt"
	"sort"
	"strings"

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
	type key struct {
		id    int64
		event string
	}
	best := map[key]Run{}
	for _, r := range rs {
		k := key{r.WorkflowID, r.Event}
		cur, seen := best[k]
		switch {
		case !seen:
		case r.Number > cur.Number:
		case r.Number == cur.Number && r.Created > cur.Created:
		default:
			continue
		}
		best[k] = r
	}
	out := make([]Run, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Name != b.Name:
			return a.Name < b.Name
		case a.Event != b.Event:
			return a.Event < b.Event
		}
		return a.WorkflowID < b.WorkflowID
	})
	return out
}

// passing reports whether a finished conclusion is accepted. NEUTRAL and
// SKIPPED are passes (a lane that excluded itself did its job); CANCELLED is
// not: a cancelled lane ran nothing.
func passing(c string) bool {
	switch strings.ToLower(c) {
	case "success", "neutral", "skipped":
		return true
	}
	return false
}

func judge(name, kind, status, conclusion string, jobs int, checkJobs bool) CheckResult {
	r := CheckResult{Name: name, Kind: kind}
	if !strings.EqualFold(status, "completed") {
		r.State, r.Detail = Pending, "status "+nz(status)
		return r
	}
	if conclusion == "" {
		// Finished, yet no conclusion read: absence is never evidence of a pass.
		r.State, r.Detail = Pending, "completed with no conclusion"
		return r
	}
	if !passing(conclusion) {
		r.State, r.Detail = Red, "conclusion "+strings.ToLower(conclusion)
		return r
	}
	if checkJobs {
		switch {
		case jobs == 0:
			r.State, r.Detail = Red, "ran with zero jobs: it never started"
			return r
		case jobs < 0:
			r.State, r.Detail = Pending, "job count not read"
			return r
		}
	}
	r.State = Green
	return r
}

func nz(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
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
		v.Checks = append(v.Checks, judge(name+" ("+r.Event+" run)", "workflow_run", r.Status, r.Conclusion, r.Jobs, true))
	}
	for _, c := range checks {
		name := c.Name
		if name == "" {
			name = "(unnamed)"
		}
		v.Checks = append(v.Checks, judge(name, "check_run", c.Status, c.Conclusion, 0, false))
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
