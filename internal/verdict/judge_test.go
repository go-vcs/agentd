package verdict

import "testing"

// A lane that is not completed is pending whatever conclusion it carries, and
// says so; a missing status is named, not blank.
func TestUnfinishedLaneIsPendingWhateverItsConclusion(t *testing.T) {
	for _, tc := range []struct{ status, conclusion, detail string }{
		{"in_progress", "success", "status in_progress"},
		{"queued", "success", "status queued"},
		{"", "success", "status (none)"},
		{"", "", "status (none)"},
	} {
		r := judgeOne("lane", "run", tc.status, tc.conclusion, 3, true)
		if r.State != Pending || r.Detail != tc.detail {
			t.Errorf("%+v: got %s %q", tc, r.State, r.Detail)
		}
	}
	if r := judgeOne("lane", "run", "COMPLETED", "success", 3, true); r.State != Green {
		t.Errorf("status is case-insensitive: %s", r.State)
	}
}
