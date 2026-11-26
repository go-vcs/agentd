package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

const (
	c1  = "1111111111111111111111111111111111111111"
	c2  = "2222222222222222222222222222222222222222"
	rid = "0123456789abcdef0123456789abcdef"
)

type fakeBE struct {
	mu       sync.Mutex
	head     string
	headErr  error
	state    verdict.State
	vErr     error
	mergeErr error
	merges   int
	verdicts atomic.Int64
	gate     chan struct{} // when set, Merge blocks on it
}

func (f *fakeBE) Verdict(_ context.Context, repo, commit string) (verdict.Verdict, error) {
	f.verdicts.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vErr != nil {
		return verdict.Verdict{}, f.vErr
	}
	return verdict.Verdict{Commit: commit, State: f.state, Checks: []verdict.CheckResult{{Name: "t", State: f.state}}}, nil
}

func (f *fakeBE) Head(context.Context, string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, f.headErr
}

func (f *fakeBE) Merge(_ context.Context, _ string, _ int, commit, _ string) (string, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges++
	return c2, f.mergeErr
}

func (f *fakeBE) set(fn func(*fakeBE)) { f.mu.Lock(); fn(f); f.mu.Unlock() }

func start(t *testing.T, be Backend, mod func(*Limits)) (*httptest.Server, *Server) {
	t.Helper()
	l := DefaultLimits()
	l.PollInterval = 5 * time.Millisecond
	if mod != nil {
		mod(&l)
	}
	s := New(be, l)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func post(t *testing.T, ts *httptest.Server, path string, body any) (int, []byte) {
	t.Helper()
	var b []byte
	switch v := body.(type) {
	case string:
		b = []byte(v)
	default:
		b, _ = json.Marshal(v)
	}
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func reason(b []byte) string {
	var e wire.ErrorBody
	if json.Unmarshal(b, &e) != nil || e.Error == nil {
		return ""
	}
	return e.Error.Reason
}

func hello(t *testing.T, ts *httptest.Server, label string) string {
	t.Helper()
	code, b := post(t, ts, "/v1/hello", wire.Hello{Label: label})
	var r wire.HelloReply
	if code != 200 || json.Unmarshal(b, &r) != nil || !wire.ValidSession(r.Session) {
		t.Fatalf("hello: %d %s", code, b)
	}
	return r.Session
}

func mergeReq(sess, id string, commit string) wire.MergeReq {
	return wire.MergeReq{Session: sess, RequestID: id, Repo: "o/r", PR: 7, Commit: commit, Method: "squash"}
}

func TestHelloSessions(t *testing.T) {
	ts, _ := start(t, &fakeBE{}, nil)
	a, a2, b := hello(t, ts, "alpha"), hello(t, ts, "alpha"), hello(t, ts, "beta")
	if a != a2 {
		t.Error("the same label got two sessions")
	}
	if a == b {
		t.Error("two labels share a session identity")
	}
	for _, l := range []string{"", strings.Repeat("x", 65), "a\nb", "a\x00b", "a\u0085b"} {
		if code, b := post(t, ts, "/v1/hello", wire.Hello{Label: l}); code != 400 || reason(b) != aerr.Malformed {
			t.Errorf("label %q: %d %s", l, code, b)
		}
	}
}

func TestSessionLimit(t *testing.T) {
	ts, _ := start(t, &fakeBE{}, func(l *Limits) { l.MaxSessions = 2 })
	hello(t, ts, "a")
	hello(t, ts, "b")
	hello(t, ts, "a") // reuse is free
	if code, b := post(t, ts, "/v1/hello", wire.Hello{Label: "c"}); code != 413 || reason(b) != aerr.BoundExceeded {
		t.Errorf("%d %s", code, b)
	}
}

func TestStrictDecoding(t *testing.T) {
	ts, _ := start(t, &fakeBE{}, nil)
	for name, body := range map[string]string{
		"unknown field": `{"label":"a","admin":true}`,
		"trailing":      `{"label":"a"}{"label":"b"}`,
		"garbage":       `nope`,
		"array":         `["a"]`,
		"empty":         ``,
	} {
		if code, b := post(t, ts, "/v1/hello", body); code != 400 || reason(b) != aerr.Malformed {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
}

func TestBodyBound(t *testing.T) {
	ts, _ := start(t, &fakeBE{}, func(l *Limits) { l.MaxBody = 64 })
	code, b := post(t, ts, "/v1/hello", `{"label":"`+strings.Repeat("a", 200)+`"}`)
	if code != 413 || reason(b) != aerr.BoundExceeded {
		t.Errorf("%d %s", code, b)
	}
}

func TestUnknownSessionAndAbbreviatedIDs(t *testing.T) {
	ts, _ := start(t, &fakeBE{state: verdict.Green}, nil)
	if code, b := post(t, ts, "/v1/verdict", wire.VerdictReq{Session: "s-" + rid, Repo: "o/r", Commit: c1}); code != 404 || reason(b) != aerr.UnknownSession {
		t.Errorf("%d %s", code, b)
	}
	s := hello(t, ts, "a")
	for _, p := range []string{"/v1/verdict", "/v1/wait-verdict"} {
		var body any = wire.VerdictReq{Session: s, Repo: "o/r", Commit: c1[:7]}
		if p == "/v1/wait-verdict" {
			body = wire.WaitReq{Session: s, Repo: "o/r", Commit: c1[:7], TimeoutSec: 1}
		}
		if code, b := post(t, ts, p, body); code != 400 || reason(b) != aerr.AbbreviatedID {
			t.Errorf("%s prefix: %d %s", p, code, b)
		}
	}
	if code, b := post(t, ts, "/v1/verdict", wire.VerdictReq{Session: s, Repo: "../x", Commit: c1}); code != 400 || reason(b) != aerr.Malformed {
		t.Errorf("repo: %d %s", code, b)
	}
}

func TestVerdictNamesCommit(t *testing.T) {
	ts, _ := start(t, &fakeBE{state: verdict.Green}, nil)
	s := hello(t, ts, "a")
	code, b := post(t, ts, "/v1/verdict", wire.VerdictReq{Session: s, Repo: "o/r", Commit: c1})
	var r wire.VerdictReply
	if code != 200 || json.Unmarshal(b, &r) != nil || r.Commit != c1 || r.State != verdict.Green {
		t.Errorf("%d %s", code, b)
	}
	be := &fakeBE{vErr: aerr.New(aerr.Unavailable, "x")}
	ts2, _ := start(t, be, nil)
	s2 := hello(t, ts2, "a")
	if code, b := post(t, ts2, "/v1/verdict", wire.VerdictReq{Session: s2, Repo: "o/r", Commit: c1}); code != 503 || reason(b) != aerr.Unavailable {
		t.Errorf("%d %s", code, b)
	}
}

func TestUnsupportedByBackend(t *testing.T) {
	ts, _ := start(t, &fakeBE{}, nil)
	for _, p := range []string{"/v1/lease/acquire", "/v1/change/create", "/v1/changeset/merge"} {
		if code, b := post(t, ts, p, `{}`); code != 501 || reason(b) != aerr.UnsupportedBackend {
			t.Errorf("%s: %d %s", p, code, b)
		}
	}
	if code, b := post(t, ts, "/v1/nope", `{}`); code != 404 || reason(b) != aerr.NotFound {
		t.Errorf("%d %s", code, b)
	}
}

func TestMergeGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(*fakeBE)
		code   int
		reason string
		merged int
	}{
		{"green and head is the commit", func(f *fakeBE) { f.head, f.state = c1, verdict.Green }, 200, "", 1},
		// amend after push: the head is now c2, the caller still names c1.
		{"head moved", func(f *fakeBE) { f.head, f.state = c2, verdict.Green }, 409, aerr.RefMoved, 0},
		{"red", func(f *fakeBE) { f.head, f.state = c1, verdict.Red }, 412, aerr.VerdictRed, 0},
		{"pending", func(f *fakeBE) { f.head, f.state = c1, verdict.Pending }, 412, aerr.VerdictPending, 0},
		{"closed", func(f *fakeBE) { f.headErr = aerr.New(aerr.ChangeClosed, "x") }, 412, aerr.ChangeClosed, 0},
		{"verdict unreadable", func(f *fakeBE) { f.head, f.vErr = c1, aerr.New(aerr.Unavailable, "x") }, 503, aerr.Unavailable, 0},
		{"github refuses: head moved at merge time", func(f *fakeBE) {
			f.head, f.state, f.mergeErr = c1, verdict.Green, aerr.New(aerr.RefMoved, "x")
		}, 409, aerr.RefMoved, 1},
	} {
		be := &fakeBE{}
		be.set(tc.set)
		ts, _ := start(t, be, nil)
		s := hello(t, ts, "a")
		code, b := post(t, ts, "/v1/merge", mergeReq(s, rid, c1))
		if code != tc.code || reason(b) != tc.reason || be.merges != tc.merged {
			t.Errorf("%s: %d %s merges=%d", tc.name, code, b, be.merges)
		}
		if tc.name == "head moved" {
			var e wire.ErrorBody
			_ = json.Unmarshal(b, &e)
			if e.Error == nil || e.Error.CurrentCommit != c2 {
				t.Errorf("head moved did not name the current commit: %s", b)
			}
		}
		if tc.name == "red" {
			var e wire.ErrorBody
			_ = json.Unmarshal(b, &e)
			if e.Error == nil || e.Error.Commit != c1 || len(e.Error.Checks) != 1 {
				t.Errorf("red did not name commit and checks: %s", b)
			}
		}
		if tc.code == 200 {
			var r wire.MergeReply
			_ = json.Unmarshal(b, &r)
			if r.Commit != c1 || r.MergeCommit != c2 {
				t.Errorf("reply %s", b)
			}
		}
	}
}

func TestMergeValidation(t *testing.T) {
	be := &fakeBE{head: c1, state: verdict.Green}
	ts, _ := start(t, be, nil)
	s := hello(t, ts, "a")
	bad := map[string]wire.MergeReq{
		"no session":   {Session: "x", RequestID: rid, Repo: "o/r", PR: 1, Commit: c1},
		"bad id":       {Session: s, RequestID: "short", Repo: "o/r", PR: 1, Commit: c1},
		"prefix":       {Session: s, RequestID: rid, Repo: "o/r", PR: 1, Commit: c1[:7]},
		"pr zero":      {Session: s, RequestID: rid, Repo: "o/r", PR: 0, Commit: c1},
		"bad method":   {Session: s, RequestID: rid, Repo: "o/r", PR: 1, Commit: c1, Method: "force"},
		"bad repo":     {Session: s, RequestID: rid, Repo: "o", PR: 1, Commit: c1},
		"upper req id": {Session: s, RequestID: strings.ToUpper(rid), Repo: "o/r", PR: 1, Commit: c1},
	}
	for name, r := range bad {
		if code, b := post(t, ts, "/v1/merge", r); code < 400 || code >= 500 {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if be.merges != 0 {
		t.Error("an invalid request merged")
	}
	// default method is accepted
	r := wire.MergeReq{Session: s, RequestID: rid, Repo: "o/r", PR: 1, Commit: c1}
	if code, b := post(t, ts, "/v1/merge", r); code != 200 {
		t.Errorf("default method: %d %s", code, b)
	}
}

func TestMergeIdempotency(t *testing.T) {
	be := &fakeBE{head: c1, state: verdict.Green}
	ts, _ := start(t, be, nil)
	s := hello(t, ts, "a")
	code1, b1 := post(t, ts, "/v1/merge", mergeReq(s, rid, c1))
	code2, b2 := post(t, ts, "/v1/merge", mergeReq(s, rid, c1))
	if code1 != 200 || code2 != 200 || !bytes.Equal(b1, b2) || be.merges != 1 {
		t.Errorf("retry: %d %d merges=%d", code1, code2, be.merges)
	}
	// same id, different request
	other := mergeReq(s, rid, c1)
	other.PR = 8
	if code, b := post(t, ts, "/v1/merge", other); code != 400 || reason(b) != aerr.RequestIDReused {
		t.Errorf("reuse: %d %s", code, b)
	}
	// same id from another session is a different write
	s2 := hello(t, ts, "b")
	if code, _ := post(t, ts, "/v1/merge", mergeReq(s2, rid, c1)); code != 200 {
		t.Errorf("other session: %d", code)
	}
}

// A failure that is retryable is not replayed: the same request id runs again.
func TestRetryableFailureIsNotRemembered(t *testing.T) {
	be := &fakeBE{head: c1, vErr: aerr.New(aerr.Unavailable, "x")}
	ts, srv := start(t, be, nil)
	s := hello(t, ts, "a")
	if code, _ := post(t, ts, "/v1/merge", mergeReq(s, rid, c1)); code != 503 {
		t.Fatal(code)
	}
	if srv.idem.size() != 0 {
		t.Error("a transient failure was remembered")
	}
	be.set(func(f *fakeBE) { f.vErr, f.state = nil, verdict.Green })
	if code, b := post(t, ts, "/v1/merge", mergeReq(s, rid, c1)); code != 200 {
		t.Errorf("retry: %d %s", code, b)
	}
	// pending is retryable too, and a terminal refusal IS remembered
	be2 := &fakeBE{head: c1, state: verdict.Pending}
	ts2, srv2 := start(t, be2, nil)
	s2 := hello(t, ts2, "a")
	post(t, ts2, "/v1/merge", mergeReq(s2, rid, c1))
	if srv2.idem.size() != 0 {
		t.Error("pending was remembered")
	}
	be3 := &fakeBE{head: c1, state: verdict.Red}
	ts3, srv3 := start(t, be3, nil)
	s3 := hello(t, ts3, "a")
	post(t, ts3, "/v1/merge", mergeReq(s3, rid, c1))
	if srv3.idem.size() != 1 {
		t.Error("a terminal refusal was not remembered")
	}
}

// Two requests for the same revision under different request ids share one
// merge: "merge already in progress" is a handle, not an error.
func TestConcurrentMergeOfOneRevisionRunsOnce(t *testing.T) {
	be := &fakeBE{head: c1, state: verdict.Green, gate: make(chan struct{})}
	ts, _ := start(t, be, nil)
	s := hello(t, ts, "a")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	ids := []string{rid, strings.Repeat("f", 32)}
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = post(t, ts, "/v1/merge", mergeReq(s, ids[i], c1))
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(be.gate)
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 || be.merges != 1 {
		t.Errorf("codes %v merges %d", codes, be.merges)
	}
}

func TestIdemTableBoundAndExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	st := newIdemStore(2, time.Hour, func() time.Time { return now })
	h := func(b byte) [32]byte { return [32]byte{b} }
	k := func(id string) idemKey { return idemKey{"s", "merge", id} }
	e1, own, err := st.begin(k("1"), h(1))
	if !own || err != nil {
		t.Fatal(own, err)
	}
	st.finish(k("1"), e1, 200, nil, true)
	e2, _, _ := st.begin(k("2"), h(2))
	st.finish(k("2"), e2, 200, nil, true)
	if _, _, err := st.begin(k("3"), h(3)); aerr.As(err).Reason != aerr.BoundExceeded {
		t.Errorf("full table: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if _, own, err := st.begin(k("3"), h(3)); !own || err != nil {
		t.Errorf("after expiry: %v %v", own, err)
	}
	// an entry still running never expires
	if _, own, _ := st.begin(k("3"), h(3)); own {
		t.Error("a running entry was taken over")
	}
	// an expired entry is re-executed
	if _, own, _ := st.begin(k("1"), h(1)); !own {
		t.Error("an expired entry was replayed")
	}
}

func TestInFlightBound(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	ts, _ := start(t, be, func(l *Limits) { l.MaxInFlight = 1 })
	s := hello(t, ts, "a")
	done := make(chan struct{})
	go func() {
		post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: 1})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	code, b := post(t, ts, "/v1/hello", wire.Hello{Label: "z"})
	if code != 413 || reason(b) != aerr.BoundExceeded {
		t.Errorf("%d %s", code, b)
	}
	<-done
}

func TestWaitBounds(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	ts, _ := start(t, be, func(l *Limits) { l.MaxWaitsSession = 1; l.MaxWatches = 1; l.MaxWait = 2 * time.Second })
	s := hello(t, ts, "a")
	for _, n := range []int{0, -1, 3} {
		if code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: n}); code != 413 || reason(b) != aerr.BoundExceeded {
			t.Errorf("timeout %d: %d %s", n, code, b)
		}
	}
	done := make(chan struct{})
	go func() {
		post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: 1})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: 1}); code != 413 || !strings.Contains(string(b), "concurrent waits") {
		t.Errorf("per-session: %d %s", code, b)
	}
	s2 := hello(t, ts, "b")
	if code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s2, Repo: "o/r", Commit: c2, TimeoutSec: 1}); code != 413 || !strings.Contains(string(b), "watched") {
		t.Errorf("watches: %d %s", code, b)
	}
	<-done
}

func TestWaitAnswersWhenDecidedAndNamesCommit(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	ts, _ := start(t, be, nil)
	s := hello(t, ts, "a")
	go func() {
		time.Sleep(50 * time.Millisecond)
		be.set(func(f *fakeBE) { f.state = verdict.Red })
	}()
	code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: 5})
	var r wire.VerdictReply
	_ = json.Unmarshal(b, &r)
	if code != 200 || r.State != verdict.Red || r.Commit != c1 || r.TimedOut {
		t.Errorf("%d %s", code, b)
	}
}

func TestWaitTimeoutIsExplicit(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	ts, _ := start(t, be, nil)
	s := hello(t, ts, "a")
	code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: 1})
	var r wire.VerdictReply
	_ = json.Unmarshal(b, &r)
	if code != 200 || r.State != verdict.Pending || !r.TimedOut || r.Commit != c1 {
		t.Errorf("%d %s", code, b)
	}
}

func TestWaitSurfacesTerminalUpstreamRefusal(t *testing.T) {
	be := &fakeBE{vErr: aerr.New(aerr.NotFound, "x")}
	ts, _ := start(t, be, nil)
	s := hello(t, ts, "a")
	if code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: 5}); code != 404 || reason(b) != aerr.NotFound {
		t.Errorf("%d %s", code, b)
	}
}

// The acceptance test of benchmark row 6: 13 sessions waiting for one commit
// cost one stream of upstream reads, not 13 pollers.
func TestThirteenWaitersCostOnePoller(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	ts, srv := start(t, be, func(l *Limits) { l.PollInterval = 20 * time.Millisecond })
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	for i := 0; i < 13; i++ {
		s := hello(t, ts, "w"+string(rune('a'+i)))
		wg.Add(1)
		ready.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			code, b := post(t, ts, "/v1/wait-verdict", wire.WaitReq{Session: s, Repo: "o/r", Commit: c1, TimeoutSec: 5})
			if code != 200 || !strings.Contains(string(b), `"GREEN"`) {
				t.Errorf("%d %s", code, b)
			}
		}()
	}
	ready.Wait()
	time.Sleep(300 * time.Millisecond)
	be.set(func(f *fakeBE) { f.state = verdict.Green })
	wg.Wait()
	// 300 ms at a 20 ms interval is about 15 reads for ONE poller; 13 pollers
	// would make about 195.
	if n := srv.Hub().Polls(); n > 40 {
		t.Errorf("%d upstream reads for 13 waiters of one commit", n)
	}
	time.Sleep(100 * time.Millisecond)
	before := srv.Hub().Polls()
	time.Sleep(100 * time.Millisecond)
	if srv.Hub().Polls() != before {
		t.Error("polling continued with no waiter")
	}
}

func TestWaiterGoneStopsPolling(t *testing.T) {
	be := &fakeBE{state: verdict.Pending}
	h := NewHub(be, 5*time.Millisecond, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	v, timedOut, err := h.Wait(ctx, "o/r", c1)
	if err != nil || !timedOut || v.Commit != c1 || v.State != verdict.Pending {
		t.Fatalf("%+v %v %v", v, timedOut, err)
	}
	time.Sleep(30 * time.Millisecond)
	n := h.Polls()
	time.Sleep(60 * time.Millisecond)
	if h.Polls() != n {
		t.Error("the poll loop outlived its last waiter")
	}
	h.mu.Lock()
	left := len(h.watches)
	h.mu.Unlock()
	if left != 0 {
		t.Errorf("%d watches left", left)
	}
}

func TestRetryableUpstreamErrorsKeepWaiting(t *testing.T) {
	be := &fakeBE{vErr: aerr.New(aerr.RateLimited, "x")}
	h := NewHub(be, 5*time.Millisecond, 4)
	go func() {
		time.Sleep(40 * time.Millisecond)
		be.set(func(f *fakeBE) { f.vErr, f.state = nil, verdict.Green })
	}()
	v, timedOut, err := h.Wait(context.Background(), "o/r", c1)
	if err != nil || timedOut || v.State != verdict.Green {
		t.Errorf("%+v %v %v", v, timedOut, err)
	}
}
