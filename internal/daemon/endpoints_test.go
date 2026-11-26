package daemon

import (
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

// Every endpoint decodes strictly and knows its session before it acts.
func TestEveryEndpointDecodesStrictlyAndChecksTheSession(t *testing.T) {
	be := &fakeBE{head: c1, state: verdict.Green}
	ts, _ := start(t, be, nil)
	sess := hello(t, ts, "a")
	ghost := "s-00000000000000000000000000000000"
	bodies := map[string]any{
		"/v1/verdict":      wire.VerdictReq{Session: ghost, Repo: "o/r", Commit: c1},
		"/v1/wait-verdict": wire.WaitReq{Session: ghost, Repo: "o/r", Commit: c1, TimeoutSec: 1},
		"/v1/merge":        mergeReq(ghost, rid, c1),
	}
	for path, body := range bodies {
		if code, b := post(t, ts, path, `{"session":"`+sess+`","bogus":1}`); code != 400 || reason(b) != aerr.Malformed {
			t.Errorf("%s unknown field: %d %s", path, code, b)
		}
		if code, b := post(t, ts, path, `nope`); code != 400 || reason(b) != aerr.Malformed {
			t.Errorf("%s garbage: %d %s", path, code, b)
		}
		if code, b := post(t, ts, path, body); reason(b) != aerr.UnknownSession {
			t.Errorf("%s unknown session: %d %s", path, code, b)
		}
	}
	if be.merges != 0 || be.verdicts.Load() != 0 {
		t.Errorf("the backend was reached: merges=%d verdicts=%d", be.merges, be.verdicts.Load())
	}
}
