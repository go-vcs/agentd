package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

// waitBound validates a client's timeout against the daemon's maximum wait.
func (s *Server) waitBound(sec int) (time.Duration, error) {
	d := time.Duration(sec) * time.Second
	if sec <= 0 || d > s.lim.MaxWait {
		return 0, aerr.New(aerr.BoundExceeded, "timeout_sec must be between 1 and the daemon's maximum wait")
	}
	return d, nil
}

// enter counts one blocking call (a wait or an event stream) of a session
// against its bound; leave must be called when it ends.
func (s *Server) enter(session string) (leave func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waits[session] >= s.lim.MaxWaitsSession {
		return nil, aerr.New(aerr.BoundExceeded, "too many concurrent waits and streams in this session")
	}
	s.waits[session]++
	return func() {
		s.mu.Lock()
		s.waits[session]--
		s.mu.Unlock()
	}, nil
}

// Cursor is the opaque name of one observation: the first 16 bytes of the
// SHA-256 of its JSON, in hex. Two observations with the same cursor are the
// same verdict, check for check.
func Cursor(v verdict.Verdict) string {
	b, _ := json.Marshal(v) // a fixed struct of strings: it cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Changes blocks until the verdict of commit differs from the observation
// named by since (empty: any observation), the verdict is final, ctx ends, or
// the upstream refuses for good. It rides the same poll loop as Wait, so any
// number of streams and waiters on one commit cost one stream of upstream reads.
//
// The cursor semantics are level-triggered: GitHub offers no event feed, so
// observations between two polls coalesce, and a caller that is slower than the
// poll interval sees the latest state, never a backlog. The final verdict is
// never skipped: it is returned at once, with final set, whatever since says.
func (h *Hub) Changes(ctx context.Context, repo, commit, since string) (v verdict.Verdict, cursor string, final, timedOut bool, err error) {
	w, release, err := h.acquire(repo, commit)
	if err != nil {
		return verdict.Verdict{}, "", false, false, err
	}
	defer release()
	for {
		h.mu.Lock()
		cur, seen, fin, ch := w.cur, w.seen, w.final, w.changed
		h.mu.Unlock()
		if seen {
			if c := Cursor(cur); fin || c != since {
				return cur, c, fin, false, nil
			}
		}
		select {
		case <-ch:
		case <-w.done:
			if w.err != nil {
				return verdict.Verdict{}, "", false, false, w.err
			}
		case <-ctx.Done():
			if !seen {
				cur = verdict.Verdict{Commit: commit, State: verdict.Pending, Checks: []verdict.CheckResult{}}
			}
			return cur, since, false, true, nil
		}
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	var req wire.EventsReq
	if err := wire.Decode(r.Body, s.lim.MaxBody, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.session(req.Session); err != nil {
		s.fail(w, err)
		return
	}
	if err := target(req.Repo, req.Commit); err != nil {
		s.fail(w, err)
		return
	}
	if req.Since != "" && !wire.ValidCursor(req.Since) {
		s.fail(w, aerr.New(aerr.Malformed, "since must be a cursor this daemon returned"))
		return
	}
	d, err := s.waitBound(req.TimeoutSec)
	if err != nil {
		s.fail(w, err)
		return
	}
	leave, err := s.enter(req.Session)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer leave()
	ctx, cancel := context.WithTimeout(r.Context(), d)
	defer cancel()
	v, cursor, final, timedOut, err := s.hub.Changes(ctx, req.Repo, req.Commit, req.Since)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ok(w, wire.EventsReply{Verdict: v, Cursor: cursor, Final: final, TimedOut: timedOut})
}
