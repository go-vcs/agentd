// Package objcache is the daemon's verified, bounded object cache
// (agent-vcs.md, "Object cache"): content-addressed by object id, shared by
// every session, and admitting an object only after its id has been recomputed
// from its content, so a corrupted or malicious upstream answer cannot poison
// it.
//
// It is bounded three ways (total bytes, entry count, one object), evicts the
// least recently used, and refuses what cannot fit rather than growing.
package objcache

import (
	"container/list"
	"context"
	"crypto/sha1" //nolint:gosec // git's object id, not a security hash of ours: the id is verified against it
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
)

// Kinds of object the cache holds.
const (
	Blob   = "blob"
	Tree   = "tree"
	Commit = "commit"
)

// Errors of Admit.
var (
	ErrMismatch = errors.New("object content does not hash to its id")
	ErrTooLarge = errors.New("object larger than the cache's per-object bound")
	ErrKind     = errors.New("unknown object kind")
	ErrID       = errors.New("object id is not a full 40 or 64 digit id")
)

// entryOverhead is charged per entry on top of its content, so that many tiny
// objects cannot hide under the byte bound.
const entryOverhead = 128

// Limits bound a cache.
type Limits struct {
	MaxBytes   int64 // content plus overhead of everything held
	MaxEntries int
	MaxObject  int64 // largest single object admitted
}

// DefaultLimits: 256 MiB, 100000 entries, 2 MiB per object. The design's
// 10 GiB default belongs to a disk-backed cache; this one lives in memory.
func DefaultLimits() Limits {
	return Limits{MaxBytes: 256 << 20, MaxEntries: 100000, MaxObject: 2 << 20}
}

// Stats are exact counters.
type Stats struct {
	Entries                 int
	Bytes                   int64
	Hits, Misses, Evictions int64
	Refused                 int64
	Fetches                 int64 // upstream fetches actually made by Fetch
}

type entry struct {
	id, kind string
	data     []byte
}

type call struct {
	done chan struct{}
	kind string
	data []byte
	err  error
}

// Cache is safe for concurrent use.
type Cache struct {
	lim Limits

	mu       sync.Mutex
	ll       *list.List // front = most recently used
	m        map[string]*list.Element
	bytes    int64
	st       Stats
	inflight map[string]*call
}

// New returns an empty cache. A zero limit is replaced by its default.
func New(l Limits) *Cache {
	d := DefaultLimits()
	if l.MaxBytes <= 0 {
		l.MaxBytes = d.MaxBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = d.MaxEntries
	}
	if l.MaxObject <= 0 {
		l.MaxObject = d.MaxObject
	}
	return &Cache{lim: l, ll: list.New(), m: map[string]*list.Element{}, inflight: map[string]*call{}}
}

// ID returns the object id git gives (kind, data): the hash of
// "<kind> <len>\0<data>", SHA-1 when idLen is 40 and SHA-256 when it is 64.
func ID(idLen int, kind string, data []byte) (string, error) {
	switch kind {
	case Blob, Tree, Commit:
	default:
		return "", ErrKind
	}
	hdr := kind + " " + strconv.Itoa(len(data)) + "\x00"
	switch idLen {
	case 40:
		h := sha1.New() //nolint:gosec
		h.Write([]byte(hdr))
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil)), nil
	case 64:
		h := sha256.New()
		h.Write([]byte(hdr))
		h.Write(data)
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	return "", ErrID
}

func cost(n int) int64 { return int64(n) + entryOverhead }

// Admit stores data under id after recomputing the id from the content. It
// refuses a mismatch, an unknown kind, and an object larger than MaxObject; it
// evicts least recently used entries to make room and refuses an object that
// would not fit even in an empty cache.
func (c *Cache) Admit(id, kind string, data []byte) error {
	if int64(len(data)) > c.lim.MaxObject || cost(len(data)) > c.lim.MaxBytes {
		c.refuse()
		return ErrTooLarge
	}
	got, err := ID(len(id), kind, data)
	if err != nil {
		c.refuse()
		return err
	}
	if got != id {
		c.refuse()
		return ErrMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[id]; ok {
		c.ll.MoveToFront(el)
		return nil
	}
	n := cost(len(data))
	for c.bytes+n > c.lim.MaxBytes || len(c.m) >= c.lim.MaxEntries {
		back := c.ll.Back()
		e := c.ll.Remove(back).(*entry)
		delete(c.m, e.id)
		c.bytes -= cost(len(e.data))
		c.st.Evictions++
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	c.m[id] = c.ll.PushFront(&entry{id: id, kind: kind, data: cp})
	c.bytes += n
	return nil
}

func (c *Cache) refuse() {
	c.mu.Lock()
	c.st.Refused++
	c.mu.Unlock()
}

// Get returns the object under id. The returned slice is shared: the caller
// must not modify it.
func (c *Cache) Get(id string) (kind string, data []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[id]
	if !ok {
		c.st.Misses++
		return "", nil, false
	}
	c.st.Hits++
	c.ll.MoveToFront(el)
	e := el.Value.(*entry)
	return e.kind, e.data, true
}

// Fetch returns the object under id, calling fetch on a miss. Concurrent
// misses of one id share one fetch. The fetched content is verified by Admit
// before it is returned or kept: an object whose content does not hash to id
// is never returned.
func (c *Cache) Fetch(ctx context.Context, id string, fetch func(context.Context) (kind string, data []byte, err error)) (string, []byte, error) {
	c.mu.Lock()
	// One look under the lock decides hit, join or lead: a second look after
	// an earlier unlocked one would be dead code, and a leader that finished
	// between the two would make this call fetch twice.
	if el, ok := c.m[id]; ok {
		c.st.Hits++
		c.ll.MoveToFront(el)
		e := el.Value.(*entry)
		c.mu.Unlock()
		return e.kind, e.data, nil
	}
	c.st.Misses++
	if cl, ok := c.inflight[id]; ok {
		c.mu.Unlock()
		select {
		case <-cl.done:
			return cl.kind, cl.data, cl.err
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
	}
	cl := &call{done: make(chan struct{})}
	c.inflight[id] = cl
	c.st.Fetches++
	c.mu.Unlock()

	// The leader's fetch is not tied to its own ctx alone being cancelled for
	// the followers: they get the leader's error and may retry.
	cl.kind, cl.data, cl.err = fetch(ctx)
	if cl.err == nil {
		if cl.err = c.Admit(id, cl.kind, cl.data); cl.err != nil {
			cl.kind, cl.data = "", nil
		}
	}
	c.mu.Lock()
	delete(c.inflight, id)
	c.mu.Unlock()
	close(cl.done)
	return cl.kind, cl.data, cl.err
}

// Stats returns the counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st
	s.Entries, s.Bytes = len(c.m), c.bytes
	return s
}
