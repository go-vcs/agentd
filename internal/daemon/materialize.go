package daemon

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
	"github.com/go-vcs/agentd/internal/materialize"
	"github.com/go-vcs/agentd/internal/objcache"
	"github.com/go-vcs/agentd/internal/wire"
)

// fetcher adapts the verified object path to materialize.Fetcher: every tree
// and blob a checkout writes has had its id recomputed.
type fetcher struct {
	s    *Server
	repo string
}

func (f fetcher) Tree(ctx context.Context, id string) ([]gittree.Entry, error) {
	es, _, err := f.s.tree(ctx, f.repo, id)
	return es, err
}

func (f fetcher) Blob(ctx context.Context, id string) ([]byte, error) {
	d, _, err := f.s.object(ctx, f.repo, objcache.Blob, id)
	return d, err
}

// commitTree returns the tree id a verified commit names. The commit is
// refused unless its first header is "tree <id>" with an id of the commit's
// own length.
func commitTree(raw []byte, idLen int) (string, error) {
	p := []byte("tree ")
	if !bytes.HasPrefix(raw, p) || len(raw) < len(p)+idLen+1 || raw[len(p)+idLen] != '\n' {
		return "", aerr.New(aerr.ObjectMismatch, "the commit object does not start with its tree header")
	}
	id := string(raw[len(p) : len(p)+idLen])
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", aerr.New(aerr.ObjectMismatch, "the commit object names a malformed tree id")
		}
	}
	return id, nil
}

func (s *Server) materialize(w http.ResponseWriter, r *http.Request) {
	var req wire.MaterializeReq
	if err := wire.Decode(r.Body, s.lim.MaxBody, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.session(req.Session); err != nil {
		s.fail(w, err)
		return
	}
	if s.lim.MaterializeRoot == "" {
		s.fail(w, aerr.New(aerr.NotPermitted, "this daemon was started without a materialize root"))
		return
	}
	if err := target(req.Repo, req.Commit); err != nil {
		s.fail(w, err)
		return
	}
	if s.mats.Add(1) > int64(s.lim.MaterializeMax) {
		s.mats.Add(-1)
		s.fail(w, aerr.New(aerr.RateLimited, "too many materializations running").RetryAfter(time.Second))
		return
	}
	defer s.mats.Add(-1)
	ctx, cancel := context.WithTimeout(r.Context(), s.lim.MaterializeTime)
	defer cancel()
	res, err := s.doMaterialize(ctx, req)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ok(w, res)
}

func (s *Server) doMaterialize(ctx context.Context, req wire.MaterializeReq) (wire.MaterializeReply, error) {
	raw, _, err := s.object(ctx, req.Repo, objcache.Commit, req.Commit)
	if err != nil {
		return wire.MaterializeReply{}, err
	}
	tree, err := commitTree(raw, len(req.Commit))
	if err != nil {
		return wire.MaterializeReply{}, err
	}
	root, err := os.OpenRoot(s.lim.MaterializeRoot)
	if err != nil {
		return wire.MaterializeReply{}, err // INTERNAL: the path stays in the daemon
	}
	defer root.Close()
	res, err := materialize.Write(ctx, root, req.Dir, tree, req.Paths, fetcher{s, req.Repo}, s.lim.Materialize)
	if err != nil {
		return wire.MaterializeReply{}, err
	}
	return wire.MaterializeReply{
		Commit: req.Commit, Tree: tree, Dir: filepath.Join(s.lim.MaterializeRoot, filepath.FromSlash(req.Dir)),
		Files: res.Files, Bytes: res.Bytes,
	}, nil
}
