package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/verdict"
)

// Backend is what forge-agentd needs of a forge. The GitHub implementation is
// internal/ghapi; the forge itself will implement it at milestone A4.
type Backend interface {
	Verdict(ctx context.Context, repo, commit string) (verdict.Verdict, error)
	// Head returns the open pull request's head commit, or an aerr.ChangeClosed.
	Head(ctx context.Context, repo string, pr int) (string, error)
	Merge(ctx context.Context, repo string, pr int, commit, method string) (string, error)
}

// Hub answers WaitVerdict from ONE poll loop per (repo, commit), however many
// local sessions wait for it: N waiters cost one stream of requests, not N
// pollers (agent-vcs.md F5, benchmark row 6).
type Hub struct {
	be       Backend
	interval time.Duration
	maxWatch int

	mu      sync.Mutex
	watches map[string]*watch
	polls   atomic.Int64
}

type watch struct {
	cancel context.CancelFunc
	done   chan struct{}
	n      int
	v      verdict.Verdict
	err    error
	last   verdict.Verdict // latest PENDING observation, for a timed-out wait

	// The event stream (events.go): the latest observation of any state, a
	// broadcast channel closed and replaced on every one, and whether the
	// observation is the final verdict.
	cur     verdict.Verdict
	seen    bool
	final   bool
	changed chan struct{}
}

// NewHub returns a hub polling every interval, with at most maxWatch distinct
// commits watched at once.
func NewHub(be Backend, interval time.Duration, maxWatch int) *Hub {
	return &Hub{be: be, interval: interval, maxWatch: maxWatch, watches: map[string]*watch{}}
}

// Polls is the number of upstream verdict reads the hub has made.
func (h *Hub) Polls() int64 { return h.polls.Load() }

// Wait blocks until the verdict of commit is not PENDING, ctx ends, or the
// upstream refuses for good. When ctx ends first it returns the last pending
// observation and timedOut.
func (h *Hub) Wait(ctx context.Context, repo, commit string) (v verdict.Verdict, timedOut bool, err error) {
	w, release, err := h.acquire(repo, commit)
	if err != nil {
		return verdict.Verdict{}, false, err
	}
	defer release()

	select {
	case <-w.done:
		return w.v, false, w.err
	case <-ctx.Done():
		h.mu.Lock()
		last := w.last
		h.mu.Unlock()
		if last.Commit == "" {
			last = verdict.Verdict{Commit: commit, State: verdict.Pending, Checks: []verdict.CheckResult{}}
		}
		return last, true, nil
	}
}

// acquire joins the poll loop of (repo, commit), starting it when this is the
// first interested caller. release must be called exactly once; the loop stops
// when the last caller has released.
func (h *Hub) acquire(repo, commit string) (*watch, func(), error) {
	key := repo + "@" + commit
	h.mu.Lock()
	w := h.watches[key]
	if w == nil {
		if len(h.watches) >= h.maxWatch {
			h.mu.Unlock()
			return nil, nil, aerr.New(aerr.BoundExceeded, "too many commits watched at once")
		}
		wctx, cancel := context.WithCancel(context.Background())
		w = &watch{cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
		h.watches[key] = w
		go h.run(wctx, key, repo, commit, w)
	}
	w.n++
	h.mu.Unlock()
	return w, func() {
		h.mu.Lock()
		w.n--
		if w.n == 0 {
			w.cancel()
			if h.watches[key] == w {
				delete(h.watches, key)
			}
		}
		h.mu.Unlock()
	}, nil
}

// observe records an observation and wakes the event streams. h.mu is held.
func (w *watch) observe(v verdict.Verdict, final bool) {
	w.cur, w.seen, w.final = v, true, final
	close(w.changed)
	w.changed = make(chan struct{})
}

func (h *Hub) run(ctx context.Context, key, repo, commit string, w *watch) {
	finish := func(v verdict.Verdict, err error) {
		h.mu.Lock()
		w.v, w.err = v, err
		if err == nil {
			w.observe(v, true)
		}
		if h.watches[key] == w {
			delete(h.watches, key)
		}
		h.mu.Unlock()
		close(w.done)
	}
	for {
		h.polls.Add(1)
		v, err := h.be.Verdict(ctx, repo, commit)
		// ctx is cancelled only when no waiter is left, so finishing a watch
		// nobody reads is harmless; the select below ends the loop.
		switch e := aerr.As(err); {
		case err == nil && v.State != verdict.Pending:
			finish(v, nil)
			return
		case err == nil:
			h.mu.Lock()
			w.last = v
			w.observe(v, false)
			h.mu.Unlock()
		case e.Retryable:
			// A 5xx or a rate limit is not an answer: keep waiting.
		default:
			finish(verdict.Verdict{}, e)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.interval):
		}
	}
}
