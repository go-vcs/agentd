package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

func evReq(sess, since string, sec int) wire.EventsReq {
	return wire.EventsReq{Session: sess, Repo: "o/r", Commit: c1, Since: since, TimeoutSec: sec}
}

func evPost(t *testing.T, ts *httptest.Server, req wire.EventsReq) wire.EventsReply {
	t.Helper()
	code, b := post(t, ts, "/v1/events", req)
	var r wire.EventsReply
	if code != 200 || json.Unmarshal(b, &r) != nil {
		t.Fatalf("%d %s", code, b)
	}
	return r
}

// pendingCursor is the cursor of the observation fakeBE gives while PENDING.
func pendingCursor() string {
	return Cursor(verdict.Verdict{Commit: c1, State: verdict.Pending, Checks: []verdict.CheckResult{{Name: "t", State: verdict.Pending}}})
}

func TestCursorNamesAnObservation(t *testing.T) {
	a := verdict.Verdict{Commit: c1, State: verdict.Pending, Checks: []verdict.CheckResult{{Name: "t", State: verdict.Pending}}}
	b := a
	if Cursor(a) != Cursor(b) || !wire.ValidCursor(Cursor(a)) {
		t.Fatal("the same observation must have one valid cursor")
	}
	b.Checks = []verdict.CheckResult{{Name: "t", State: verdict.Pending}, {Name: "u", State: verdict.Pending}}
	if Cursor(a) == Cursor(b) {
		t.Error("an added check did not change the cursor")
	}
	b = a
	b.State = verdict.Green
	if Cursor(a) == Cursor(b) {
		t.Error("a state change did not change the cursor")
	}
}

func TestEventsDeliverEachChangeThenTheFinal(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	ts, _ := start(t, be, nil)
	s := hello(t, ts, "a")
	first := evPost(t, ts, evReq(s, "", 5))
	if first.State != verdict.Pending || first.Final || first.TimedOut || !wire.ValidCursor(first.Cursor) {
		t.Fatalf("first: %+v", first)
	}
	// Nothing changed: the call waits its whole timeout and says so.
	quiet := evPost(t, ts, evReq(s, first.Cursor, 1))
	if !quiet.TimedOut || quiet.Cursor != first.Cursor || quiet.State != verdict.Pending {
		t.Fatalf("quiet: %+v", quiet)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		be.set(func(f *fakeBE) { f.state = verdict.Green })
	}()
	last := evPost(t, ts, evReq(s, first.Cursor, 5))
	if last.State != verdict.Green || !last.Final || last.TimedOut || last.Cursor == first.Cursor {
		t.Fatalf("last: %+v", last)
	}
	// The final verdict is never skipped, whatever cursor the caller holds.
	again := evPost(t, ts, evReq(s, last.Cursor, 5))
	if again.State != verdict.Green || !again.Final || again.Cursor != last.Cursor {
		t.Fatalf("again: %+v", again)
	}
}

func TestEventsRefusals(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	ts, _ := start(t, be, func(l *Limits) { l.MaxWait = 10 * time.Second })
	s := hello(t, ts, "a")
	cases := []struct {
		name string
		body any
		code int
		want string
	}{
		{"bad body", `nope`, 400, aerr.Malformed},
		{"unknown session", evReq("s-"+strings.Repeat("0", 32), "", 1), 404, aerr.UnknownSession},
		{"bad repo", wire.EventsReq{Session: s, Repo: "../x", Commit: c1, TimeoutSec: 1}, 400, aerr.Malformed},
		{"abbreviated commit", wire.EventsReq{Session: s, Repo: "o/r", Commit: c1[:10], TimeoutSec: 1}, 400, aerr.AbbreviatedID},
		{"uppercase cursor", evReq(s, strings.Repeat("A", 32), 1), 400, aerr.Malformed},
		{"short cursor", evReq(s, "abc", 1), 400, aerr.Malformed},
		{"zero timeout", evReq(s, "", 0), 413, aerr.BoundExceeded},
		{"timeout over the maximum", evReq(s, "", 11), 413, aerr.BoundExceeded},
	}
	for _, tc := range cases {
		code, b := post(t, ts, "/v1/events", tc.body)
		if code != tc.code || reason(b) != tc.want {
			t.Errorf("%s: %d %s", tc.name, code, b)
		}
	}
	// A timeout exactly at the maximum is allowed.
	if r := evPost(t, ts, evReq(s, "", 10)); r.State != verdict.Pending {
		t.Errorf("at the maximum: %+v", r)
	}
}

func TestEventsSurfaceATerminalUpstreamRefusal(t *testing.T) {
	be := &fakeBE{vErr: aerr.New(aerr.NotFound, "x")}
	ts, _ := start(t, be, nil)
	s := hello(t, ts, "a")
	if code, b := post(t, ts, "/v1/events", evReq(s, "", 5)); code != 404 || reason(b) != aerr.NotFound {
		t.Errorf("%d %s", code, b)
	}
}

// Streams count against the same per-session bound as waits, and release it.
func TestEventsAreBoundedPerSession(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	ts, srv := start(t, be, func(l *Limits) { l.MaxWaitsSession = 1 })
	s := hello(t, ts, "a")
	first := evPost(t, ts, evReq(s, "", 5))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); post(t, ts, "/v1/events", evReq(s, first.Cursor, 2)) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		n := srv.waits[s]
		srv.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code, b := post(t, ts, "/v1/events", evReq(s, first.Cursor, 1)); code != 413 || reason(b) != aerr.BoundExceeded {
		t.Errorf("second stream: %d %s", code, b)
	}
	if code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: 1}); code != 413 || reason(b) != aerr.BoundExceeded {
		t.Errorf("a wait beside the stream: %d %s", code, b)
	}
	wg.Wait()
	srv.mu.Lock()
	n := srv.waits[s]
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("%d blocking calls still counted", n)
	}
}

func TestChangesStopsPollingWithItsLastStream(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	h := NewHub(be, 5*time.Millisecond, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	v, cur, final, timedOut, err := h.Changes(ctx, "o/r", c1, pendingCursor())
	if err != nil || !timedOut || final || v.State != verdict.Pending || cur == "" {
		t.Fatalf("%+v %q %v %v %v", v, cur, final, timedOut, err)
	}
	time.Sleep(30 * time.Millisecond)
	n := h.Polls()
	time.Sleep(60 * time.Millisecond)
	if h.Polls() != n {
		t.Error("the poll loop outlived its last stream")
	}
	h.mu.Lock()
	left := len(h.watches)
	h.mu.Unlock()
	if left != 0 {
		t.Errorf("%d watches left", left)
	}
}

func TestChangesBeforeTheFirstObservationTimesOutPending(t *testing.T) {
	be := &fakeBE{state: verdict.Pending, vErr: aerr.New(aerr.RateLimited, "x")}
	h := NewHub(be, 5*time.Millisecond, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	v, cur, final, timedOut, err := h.Changes(ctx, "o/r", c1, "")
	if err != nil || !timedOut || final || cur != "" || v.Commit != c1 || v.State != verdict.Pending || v.Checks == nil {
		t.Fatalf("%+v %q %v %v %v", v, cur, final, timedOut, err)
	}
}

func TestChangesRefusesTooManyCommits(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	h := NewHub(be, 5*time.Millisecond, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.Changes(ctx, "o/r", c1, pendingCursor()) }()
	deadline := time.Now().Add(5 * time.Second)
	for h.Polls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, _, _, _, err := h.Changes(ctx, "o/r", c2, ""); aerr.As(err).Reason != aerr.BoundExceeded {
		t.Errorf("%v", err)
	}
	<-done
}

// Benchmark row 6, counted not timed: 13 sessions waiting AND 13 sessions
// streaming events for one commit cost one stream of upstream reads. The table
// is logged (go test -v) and its bound asserted.
func TestThirteenWaitersAndThirteenStreamsCostOnePoller(t *testing.T) {
	const n = 13
	const interval = 20 * time.Millisecond
	const window = 400 * time.Millisecond
	be := &fakeBE{state: verdict.Pending}
	ts, srv := start(t, be, func(l *Limits) { l.PollInterval = interval })
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		ws, es := hello(t, ts, "w"+string(rune('a'+i))), hello(t, ts, "e"+string(rune('a'+i)))
		wg.Add(2)
		go func() {
			defer wg.Done()
			if code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: ws, Repo: "o/r", Commit: c1, TimeoutSec: 10}); code != 200 || !strings.Contains(string(b), `"GREEN"`) {
				t.Errorf("wait: %d %s", code, b)
			}
		}()
		go func() {
			defer wg.Done()
			var since string
			for {
				code, b := post(t, ts, "/v1/events", evReq(es, since, 10))
				var r wire.EventsReply
				if code != 200 || json.Unmarshal(b, &r) != nil {
					t.Errorf("events: %d %s", code, b)
					return
				}
				if r.Final {
					if r.State != verdict.Green {
						t.Errorf("final: %+v", r)
					}
					return
				}
				since = r.Cursor
			}
		}()
	}
	time.Sleep(window)
	be.set(func(f *fakeBE) { f.state = verdict.Green })
	wg.Wait()
	ticks := int64(window/interval) + 3 // the window, the final read, and slack for scheduling
	polls := srv.Hub().Polls()
	// The baseline, counted the same way: 26 independent pollers, each reading the
	// verdict every interval over the same window.
	base := &fakeBE{state: verdict.Pending}
	var pw sync.WaitGroup
	for i := 0; i < 2*n; i++ {
		pw.Add(1)
		go func() {
			defer pw.Done()
			for end := time.Now().Add(window); time.Now().Before(end); time.Sleep(interval) {
				_, _ = base.Verdict(context.Background(), "o/r", c1)
			}
		}()
	}
	pw.Wait()
	independent := base.verdicts.Load()
	t.Logf("| waiters | streams | upstream reads through the daemon | upstream reads, 26 independent pollers |")
	t.Logf("| %d | %d | %d | %d |", n, n, polls, independent)
	t.Logf("at the default 15 s interval: %d reads/hour through the daemon, %d reads/hour for %d independent pollers",
		3600/15, 3600/15*2*n, 2*n)
	if independent < 10*polls/2 {
		t.Errorf("the baseline made %d reads against the daemon's %d: the comparison measures nothing", independent, polls)
	}
	if polls > ticks+10 {
		t.Errorf("%d upstream reads for %d waiters and %d streams of one commit (bound %d)", polls, n, n, ticks+10)
	}
}
