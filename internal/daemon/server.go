// Package daemon is forge-agentd: the local socket API that agents call, and
// the GitHub-backed implementation behind it (milestone A0 of
// forge docs/agent-vcs.md).
//
// Agents never hold a token: the daemon holds the only credential, inside its
// Backend. The protocol is JSON over HTTP/1.1 on a Unix socket whose peers are
// checked by uid.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/ghapi"
	"github.com/go-vcs/agentd/internal/oid"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

// Limits are the daemon's bounds; each is refused with a test.
type Limits struct {
	MaxBody         int64         // bytes of one request
	MaxInFlight     int           // concurrent requests ("local clients")
	MaxSessions     int           // distinct labels
	MaxWaitsSession int           // concurrent waits of one session
	MaxWatches      int           // distinct commits polled at once
	MaxWait         time.Duration // longest single wait
	MaxIdemRows     int           // remembered writes
	IdemTTL         time.Duration
	MergeTimeout    time.Duration
	PollInterval    time.Duration
}

// DefaultLimits are agent-vcs.md's local-daemon bounds where it states one
// (64 local clients) and conservative values elsewhere.
func DefaultLimits() Limits {
	return Limits{
		MaxBody: 64 << 10, MaxInFlight: 64, MaxSessions: 64, MaxWaitsSession: 16,
		MaxWatches: 256, MaxWait: time.Hour, MaxIdemRows: 100000, IdemTTL: 24 * time.Hour,
		MergeTimeout: 2 * time.Minute, PollInterval: 15 * time.Second,
	}
}

// Server is the local API.
type Server struct {
	be   Backend
	hub  *Hub
	lim  Limits
	now  func() time.Time
	idem *idemStore

	inflight atomic.Int64

	mu       sync.Mutex
	sessions map[string]string // label -> session id
	known    map[string]bool   // session id
	waits    map[string]int    // session id -> concurrent waits
	merging  map[string]*mergeOp
}

type mergeOp struct {
	done   chan struct{}
	status int
	body   []byte
}

// New returns a server over be.
func New(be Backend, lim Limits) *Server {
	now := time.Now
	return &Server{
		be: be, lim: lim, now: now,
		hub:      NewHub(be, lim.PollInterval, lim.MaxWatches),
		idem:     newIdemStore(lim.MaxIdemRows, lim.IdemTTL, now),
		sessions: map[string]string{}, known: map[string]bool{},
		waits: map[string]int{}, merging: map[string]*mergeOp{},
	}
}

// Hub exposes the poll hub (for the acceptance counts).
func (s *Server) Hub() *Hub { return s.hub }

// Handler returns the HTTP handler of the local API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/hello", s.hello)
	mux.HandleFunc("POST /v1/verdict", s.verdict)
	mux.HandleFunc("POST /v1/wait-verdict", s.wait)
	mux.HandleFunc("POST /v1/merge", s.merge)
	// Operations whose guarantees need the forge answer UNSUPPORTED_BY_BACKEND,
	// never a weaker imitation (agent-vcs.md, milestone A0).
	mux.HandleFunc("POST /v1/lease/", s.unsupported)
	mux.HandleFunc("POST /v1/change/", s.unsupported)
	mux.HandleFunc("POST /v1/changeset/", s.unsupported)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, aerr.New(aerr.NotFound, "no such endpoint"))
	})
	return s.bound(mux)
}

func (s *Server) bound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.inflight.Add(1) > int64(s.lim.MaxInFlight) {
			s.inflight.Add(-1)
			s.fail(w, aerr.New(aerr.BoundExceeded, "too many concurrent requests").RetryAfter(time.Second))
			return
		}
		defer s.inflight.Add(-1)
		r.Body = http.MaxBytesReader(w, r.Body, s.lim.MaxBody)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) reply(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Two writes, never append: body may be shared by several replays.
	_, _ = w.Write(body)
	_, _ = w.Write([]byte{'\n'})
}

func (s *Server) ok(w http.ResponseWriter, v any) {
	// v is always one of this module's fixed wire structs: it cannot fail to marshal.
	b, _ := json.Marshal(v)
	s.reply(w, http.StatusOK, b)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	e := aerr.As(err)
	b, _ := json.Marshal(wire.ErrorBody{Error: e})
	s.reply(w, e.HTTPStatus(), b)
}

func (s *Server) unsupported(w http.ResponseWriter, r *http.Request) {
	s.fail(w, aerr.New(aerr.UnsupportedBackend, "the GitHub backend cannot give this guarantee; it needs the forge"))
}

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	var req wire.Hello
	if err := wire.Decode(r.Body, s.lim.MaxBody, &req); err != nil {
		s.fail(w, err)
		return
	}
	if !wire.ValidLabel(req.Label) {
		s.fail(w, aerr.New(aerr.Malformed, "label must be 1 to 64 printable characters"))
		return
	}
	s.mu.Lock()
	id, ok := s.sessions[req.Label]
	if !ok {
		if len(s.sessions) >= s.lim.MaxSessions {
			s.mu.Unlock()
			s.fail(w, aerr.New(aerr.BoundExceeded, "too many sessions"))
			return
		}
		var b [16]byte
		_, _ = rand.Read(b[:]) // never fails (Go 1.24 aborts instead)
		id = "s-" + hex.EncodeToString(b[:])
		s.sessions[req.Label] = id
		s.known[id] = true
	}
	s.mu.Unlock()
	s.ok(w, wire.HelloReply{Session: id, Label: req.Label})
}

func (s *Server) session(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.known[id] {
		return aerr.New(aerr.UnknownSession, "say hello first")
	}
	return nil
}

// target validates the common fields of a read.
func target(repo, commit string) error {
	if !ghapi.ValidRepo(repo) {
		return aerr.New(aerr.Malformed, "repo must be owner/name")
	}
	if _, err := oid.Parse(commit); err != nil {
		if err == oid.ErrAbbreviated {
			return aerr.New(aerr.AbbreviatedID, "a full object id is required; prefixes are never expanded")
		}
		return aerr.New(aerr.Malformed, "malformed object id")
	}
	return nil
}

func (s *Server) verdict(w http.ResponseWriter, r *http.Request) {
	var req wire.VerdictReq
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
	v, err := s.be.Verdict(r.Context(), req.Repo, req.Commit)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ok(w, wire.VerdictReply{Verdict: v})
}

func (s *Server) wait(w http.ResponseWriter, r *http.Request) {
	var req wire.WaitReq
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
	d := time.Duration(req.TimeoutSec) * time.Second
	if req.TimeoutSec <= 0 || d > s.lim.MaxWait {
		s.fail(w, aerr.New(aerr.BoundExceeded, "timeout_sec must be between 1 and the daemon's maximum wait"))
		return
	}
	s.mu.Lock()
	if s.waits[req.Session] >= s.lim.MaxWaitsSession {
		s.mu.Unlock()
		s.fail(w, aerr.New(aerr.BoundExceeded, "too many concurrent waits in this session"))
		return
	}
	s.waits[req.Session]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.waits[req.Session]--
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(r.Context(), d)
	defer cancel()
	v, timedOut, err := s.hub.Wait(ctx, req.Repo, req.Commit)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ok(w, wire.VerdictReply{Verdict: v, TimedOut: timedOut})
}

func (s *Server) merge(w http.ResponseWriter, r *http.Request) {
	var req wire.MergeReq
	if err := wire.Decode(r.Body, s.lim.MaxBody, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.session(req.Session); err != nil {
		s.fail(w, err)
		return
	}
	if !wire.ValidRequestID(req.RequestID) {
		s.fail(w, aerr.New(aerr.Malformed, "request_id must be 32 lowercase hex digits (16 random bytes)"))
		return
	}
	if err := target(req.Repo, req.Commit); err != nil {
		s.fail(w, err)
		return
	}
	if req.PR <= 0 {
		s.fail(w, aerr.New(aerr.Malformed, "pr must be positive"))
		return
	}
	if req.Method == "" {
		req.Method = "merge"
	}
	if req.Method != "merge" && req.Method != "squash" && req.Method != "rebase" {
		s.fail(w, aerr.New(aerr.Malformed, "method must be merge, squash or rebase"))
		return
	}
	canon, _ := json.Marshal(req)
	k := idemKey{req.Session, "merge", req.RequestID}
	e, owner, err := s.idem.begin(k, sha256.Sum256(canon))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !owner {
		select {
		case <-e.done:
			s.reply(w, e.status, e.body)
		case <-r.Context().Done():
		}
		return
	}
	status, body := s.mergeOnce(req)
	// A retryable refusal is not remembered: the retry must run again.
	keep := status < 500 && status != http.StatusTooManyRequests
	if status == http.StatusPreconditionFailed {
		keep = !isRetryable(body)
	}
	s.idem.finish(k, e, status, body, keep)
	s.reply(w, status, body)
}

func isRetryable(body []byte) bool {
	var b wire.ErrorBody
	return json.Unmarshal(body, &b) == nil && b.Error != nil && b.Error.Retryable
}

// mergeOnce runs the merge of one (repo, pr, commit) at most once at a time:
// a second request for the same revision returns the first one's result,
// whatever its request id ("merge already in progress" is a handle, not an
// error).
func (s *Server) mergeOnce(req wire.MergeReq) (int, []byte) {
	key := req.Repo + "#" + itoa(req.PR) + "@" + req.Commit
	s.mu.Lock()
	if op, ok := s.merging[key]; ok {
		s.mu.Unlock()
		<-op.done
		return op.status, op.body
	}
	op := &mergeOp{done: make(chan struct{})}
	s.merging[key] = op
	s.mu.Unlock()

	// Not the request's context: a client that hangs up must not abandon a
	// merge half way, and its retry finds this result.
	ctx, cancel := context.WithTimeout(context.Background(), s.lim.MergeTimeout)
	res, err := s.doMerge(ctx, req)
	cancel()
	if err != nil {
		e := aerr.As(err)
		op.status = e.HTTPStatus()
		op.body, _ = json.Marshal(wire.ErrorBody{Error: e})
	} else {
		op.status = http.StatusOK
		op.body, _ = json.Marshal(res)
	}
	s.mu.Lock()
	delete(s.merging, key)
	s.mu.Unlock()
	close(op.done)
	return op.status, op.body
}

// doMerge is the merge gate: it merges only the exact head commit the caller
// named, and only if that commit is green.
func (s *Server) doMerge(ctx context.Context, req wire.MergeReq) (wire.MergeReply, error) {
	head, err := s.be.Head(ctx, req.Repo, req.PR)
	if err != nil {
		return wire.MergeReply{}, err
	}
	if head != req.Commit {
		// HEAD moved: the "amend after push" case. The caller must re-read and
		// get its new commit judged; nothing is merged.
		e := aerr.New(aerr.RefMoved, "the pull request head is not the commit named")
		e.CurrentCommit = head
		return wire.MergeReply{}, e
	}
	v, err := s.be.Verdict(ctx, req.Repo, req.Commit)
	if err != nil {
		return wire.MergeReply{}, err
	}
	switch v.State {
	case verdict.Green:
	case verdict.Red:
		e := aerr.New(aerr.VerdictRed, "a check of this exact commit failed")
		e.Commit, e.Checks = v.Commit, v.Checks
		return wire.MergeReply{}, e
	default:
		e := aerr.New(aerr.VerdictPending, "this exact commit is not green yet")
		e.Commit, e.Checks = v.Commit, v.Checks
		return wire.MergeReply{}, e
	}
	// GitHub applies the sha precondition atomically with the merge: if the head
	// moved since the read above it answers 409, reported as REF_MOVED.
	mc, err := s.be.Merge(ctx, req.Repo, req.PR, req.Commit, req.Method)
	if err != nil {
		return wire.MergeReply{}, err
	}
	return wire.MergeReply{Repo: req.Repo, PR: req.PR, Commit: req.Commit, MergeCommit: mc}, nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
