package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
	"github.com/go-vcs/agentd/internal/objcache"
	"github.com/go-vcs/agentd/internal/wire"
)

// ObjectBackend is the optional half of a Backend that serves git objects. It
// returns the raw content (what git hashes after its header) and is NOT
// trusted: the cache recomputes the id before anything is kept or returned.
type ObjectBackend interface {
	Object(ctx context.Context, repo, kind, id string) ([]byte, error)
}

// Cache exposes the object cache (for the acceptance counts).
func (s *Server) Cache() *objcache.Cache { return s.cache }

// object returns the verified object id of the given kind, from the cache or
// fetched once from the upstream, and whether it was a cache hit.
func (s *Server) object(ctx context.Context, repo, kind, id string) ([]byte, bool, error) {
	if s.objs == nil {
		return nil, false, aerr.New(aerr.UnsupportedBackend, "this backend does not serve objects")
	}
	if k, d, ok := s.cache.Get(id); ok && k == kind {
		return d, true, nil
	}
	k, d, err := s.cache.Fetch(ctx, id, func(ctx context.Context) (string, []byte, error) {
		d, err := s.objs.Object(ctx, repo, kind, id)
		return kind, d, err
	})
	if err != nil {
		switch {
		case errors.Is(err, objcache.ErrMismatch):
			return nil, false, aerr.New(aerr.ObjectMismatch, "the upstream's bytes do not hash to the id asked for; nothing was kept")
		case errors.Is(err, objcache.ErrTooLarge):
			return nil, false, aerr.New(aerr.BoundExceeded, "object larger than the cache's per-object bound")
		}
		return nil, false, err
	}
	if k != kind {
		// The id is in the cache as another kind: asking for the wrong one.
		return nil, false, aerr.New(aerr.NotFound, "no "+kind+" has this id")
	}
	return d, false, nil
}

func (s *Server) objectReq(w http.ResponseWriter, r *http.Request) (wire.ObjectReq, bool) {
	var req wire.ObjectReq
	if err := wire.Decode(r.Body, s.lim.MaxBody, &req); err != nil {
		s.fail(w, err)
		return req, false
	}
	if err := s.session(req.Session); err != nil {
		s.fail(w, err)
		return req, false
	}
	if err := target(req.Repo, req.ID); err != nil {
		s.fail(w, err)
		return req, false
	}
	return req, true
}

func (s *Server) readBlob(w http.ResponseWriter, r *http.Request) {
	req, ok := s.objectReq(w, r)
	if !ok {
		return
	}
	d, hit, err := s.object(r.Context(), req.Repo, objcache.Blob, req.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ok(w, wire.BlobReply{ID: req.ID, Size: len(d), Content: base64.StdEncoding.EncodeToString(d), Cached: hit})
}

func (s *Server) readTree(w http.ResponseWriter, r *http.Request) {
	req, ok := s.objectReq(w, r)
	if !ok {
		return
	}
	es, hit, err := s.tree(r.Context(), req.Repo, req.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ok(w, wire.TreeReply{ID: req.ID, Entries: es, Cached: hit})
}

// tree returns the parsed, verified tree id.
func (s *Server) tree(ctx context.Context, repo, id string) ([]gittree.Entry, bool, error) {
	d, hit, err := s.object(ctx, repo, objcache.Tree, id)
	if err != nil {
		return nil, false, err
	}
	es, err := gittree.Parse(d, len(id)/2)
	if err != nil {
		// Hashes to its id yet is not a tree git would write: refuse, never guess.
		return nil, false, aerr.New(aerr.ObjectMismatch, "the tree object is malformed")
	}
	return es, hit, nil
}
