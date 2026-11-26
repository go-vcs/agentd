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
	key := repo + "@" + commit
	h.mu.Lock()
	w := h.watches[key]
	if w == nil {
		if len(h.watches) >= h.maxWatch {
			h.mu.Unlock()
			return verdict.Verdict{}, false, aerr.New(aerr.BoundExceeded, "too many commits watched at once")
		}
		wctx, cancel := context.WithCancel(context.Background())
		w = &watch{cancel: cancel, done: make(chan struct{})}
		h.watches[key] = w
		go h.run(wctx, key, repo, commit, w)
	}
	w.n++
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		w.n--
		if w.n == 0 {
			w.cancel()
			if h.watches[key] == w {
				delete(h.watches, key)
			}
		}
		h.mu.Unlock()
	}()

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

func (h *Hub) run(ctx context.Context, key, repo, commit string, w *watch) {
	finish := func(v verdict.Verdict, err error) {
		h.mu.Lock()
		w.v, w.err = v, err
		if h.watches[key] == w {
			delete(h.watches, key)
		}
		h.mu.Unlock()
		close(w.done)
	}
	for {
		h.polls.Add(1)
		v, err := h.be.Verdict(ctx, repo, commit)
		if ctx.Err() != nil {
			return
		}
		switch e := aerr.As(err); {
		case err == nil && v.State != verdict.Pending:
			finish(v, nil)
			return
		case err == nil:
			h.mu.Lock()
			w.last = v
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
