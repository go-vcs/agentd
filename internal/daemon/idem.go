package daemon

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
)

// idemKey identifies one write: the same request id from two sessions is two
// writes, never one.
type idemKey struct{ session, method, id string }

type idemEntry struct {
	hash   [sha256.Size]byte
	done   chan struct{}
	status int
	body   []byte
	at     time.Time
}

// idemStore keeps (session, method, request id) -> (hash of the request,
// response) so that a retry is always safe (agent-vcs.md "Idempotency").
type idemStore struct {
	mu  sync.Mutex
	m   map[idemKey]*idemEntry
	max int
	ttl time.Duration
	now func() time.Time
}

func newIdemStore(max int, ttl time.Duration, now func() time.Time) *idemStore {
	return &idemStore{m: map[idemKey]*idemEntry{}, max: max, ttl: ttl, now: now}
}

// begin returns the entry for k and whether the caller owns its execution. A
// second caller with the same request hash waits on entry.done; one with a
// different hash is refused.
func (s *idemStore) begin(k idemKey, hash [sha256.Size]byte) (*idemEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[k]; ok {
		if s.expired(e) {
			delete(s.m, k)
		} else {
			if e.hash != hash {
				return nil, false, aerr.New(aerr.RequestIDReused, "this request id was used for a different request")
			}
			return e, false, nil
		}
	}
	if len(s.m) >= s.max {
		for kk, e := range s.m {
			if s.expired(e) {
				delete(s.m, kk)
			}
		}
		if len(s.m) >= s.max {
			return nil, false, aerr.New(aerr.BoundExceeded, "idempotency table full")
		}
	}
	e := &idemEntry{hash: hash, done: make(chan struct{}), at: s.now()}
	s.m[k] = e
	return e, true, nil
}

func (s *idemStore) expired(e *idemEntry) bool {
	select {
	case <-e.done:
		return s.now().Sub(e.at) > s.ttl
	default:
		return false
	}
}

// finish records the response. A retryable failure is forgotten, so that the
// same request id runs again instead of replaying a transient error.
func (s *idemStore) finish(k idemKey, e *idemEntry, status int, body []byte, keep bool) {
	s.mu.Lock()
	e.status, e.body = status, body
	if !keep {
		delete(s.m, k)
	}
	s.mu.Unlock()
	close(e.done)
}

func (s *idemStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
