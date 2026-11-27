// Package materialize writes a sparse checkout of a verified tree into a
// directory under a root the daemon owns (agent-vcs.md, "Materialize"): a plain
// directory, not a git worktree.
//
// Everything written goes through an os.Root scoped to the new directory, so a
// symbolic link inside the checkout can never lead a later write outside it;
// names git itself refuses to check out (".git") are refused, a symlink whose
// target leaves the checkout is refused, and the number of files, bytes, trees
// and the depth are bounded. A failure removes what was written: the directory
// exists afterwards only if every requested path was written.
package materialize

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
)

// Fetcher returns verified objects: a tree's entries and a blob's bytes.
type Fetcher interface {
	Tree(ctx context.Context, id string) ([]gittree.Entry, error)
	Blob(ctx context.Context, id string) ([]byte, error)
}

// Limits bound one materialization.
type Limits struct {
	MaxFiles int   // regular files and symlinks written
	MaxBytes int64 // bytes of file content written
	MaxPaths int   // entries of the sparse path list
	MaxDepth int   // directory depth below the destination
	MaxTrees int   // trees fetched
}

// DefaultLimits: 20000 files, 256 MiB, 256 paths, depth 32, 20000 trees.
func DefaultLimits() Limits {
	return Limits{MaxFiles: 20000, MaxBytes: 256 << 20, MaxPaths: 256, MaxDepth: 32, MaxTrees: 20000}
}

// MaxDirDepth and MaxNameLen bound the destination name and every path element.
const (
	MaxDirDepth = 8
	MaxNameLen  = 255
	MaxPathLen  = 4096
)

// Result counts what was written.
type Result struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// SafeName reports whether one path element may be created: git's own rules
// (no empty, ".", "..", slash or NUL; checked by gittree for entries) plus the
// names a checkout must never contain.
func SafeName(n string) bool {
	if n == "" || n == "." || n == ".." || len(n) > MaxNameLen {
		return false
	}
	if strings.EqualFold(n, ".git") || strings.EqualFold(n, "git~1") {
		return false
	}
	return !strings.ContainsAny(n, "/\\\x00")
}

// Elems splits a slash-separated relative path into elements, refusing an
// absolute path, a trailing slash, and any element SafeName refuses.
func Elems(p string, maxElems int) ([]string, error) {
	if len(p) > MaxPathLen { // an empty path splits into one empty element, refused below
		return nil, aerr.New(aerr.Malformed, "path is too long")
	}
	es := strings.Split(p, "/")
	if len(es) > maxElems {
		return nil, aerr.New(aerr.BoundExceeded, "path is too deep")
	}
	for _, e := range es {
		if !SafeName(e) {
			return nil, aerr.New(aerr.Malformed, "path holds an element a checkout cannot contain")
		}
	}
	return es, nil
}

type node struct {
	all      bool
	children map[string]*node
}

func (n *node) add(es []string) {
	for _, e := range es {
		if n.children == nil {
			n.children = map[string]*node{}
		}
		c, ok := n.children[e]
		if !ok {
			c = &node{}
			n.children[e] = c
		}
		n = c
	}
	n.all = true
}

// fsRoot is the part of *os.Root the checkout uses, so that a test can make
// any one operation fail and see that the output is removed and the error kept.
type fsRoot interface {
	MkdirAll(name string, perm os.FileMode) error
	Mkdir(name string, perm os.FileMode) error
	OpenFile(name string, flag int, perm os.FileMode) (io.WriteCloser, error)
	Symlink(oldname, newname string) error
	RemoveAll(name string) error
	OpenRoot(name string) (fsRoot, error)
	Close() error
}

type osRoot struct{ *os.Root }

func (r osRoot) OpenFile(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	return r.Root.OpenFile(name, flag, perm)
}

func (r osRoot) OpenRoot(name string) (fsRoot, error) {
	s, err := r.Root.OpenRoot(name)
	return osRoot{s}, err
}

type writer struct {
	ctx   context.Context
	r     fsRoot
	f     Fetcher
	lim   Limits
	res   Result
	trees int
}

// Write materializes the tree treeID, restricted to paths (empty: the whole
// tree), as the new directory dir under root. dir must not exist.
func Write(ctx context.Context, root *os.Root, dir, treeID string, paths []string, f Fetcher, lim Limits) (Result, error) {
	return write(ctx, osRoot{root}, dir, treeID, paths, f, lim)
}

func write(ctx context.Context, root fsRoot, dir, treeID string, paths []string, f Fetcher, lim Limits) (Result, error) {
	des, err := Elems(dir, MaxDirDepth)
	if err != nil {
		return Result{}, err
	}
	if len(paths) > lim.MaxPaths {
		return Result{}, aerr.New(aerr.BoundExceeded, "too many paths")
	}
	sel := &node{}
	if len(paths) == 0 {
		sel.all = true
	}
	for _, p := range paths {
		es, err := Elems(p, lim.MaxDepth)
		if err != nil {
			return Result{}, err
		}
		sel.add(es)
	}
	if len(des) > 1 {
		if err := root.MkdirAll(path.Join(des[:len(des)-1]...), 0o755); err != nil {
			return Result{}, fsError(err)
		}
	}
	if err := root.Mkdir(dir, 0o755); err != nil {
		return Result{}, fsError(err) // an existing destination is CONFLICT
	}
	sub, err := root.OpenRoot(dir)
	if err != nil {
		_ = root.RemoveAll(dir)
		return Result{}, fsError(err)
	}
	defer sub.Close()
	w := &writer{ctx: ctx, r: sub, f: f, lim: lim}
	if err := w.tree(treeID, nil, sel); err != nil {
		_ = root.RemoveAll(dir)
		return Result{}, err
	}
	return w.res, nil
}

// fsError maps a filesystem error; nil stays nil.
func fsError(err error) error {
	if errors.Is(err, fs.ErrExist) {
		return aerr.New(aerr.Conflict, "a path of the checkout already exists (a name differing only in case?)")
	}
	return err // not an aerr.Error: reported as INTERNAL, the cause stays in the daemon
}

func (w *writer) tree(id string, rel []string, sel *node) error {
	if len(rel) > w.lim.MaxDepth {
		return aerr.New(aerr.BoundExceeded, "tree is deeper than the bound")
	}
	if w.trees++; w.trees > w.lim.MaxTrees {
		return aerr.New(aerr.BoundExceeded, "too many trees")
	}
	es, err := w.f.Tree(w.ctx, id)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(es))
	for _, e := range es {
		if seen[e.Name] {
			return aerr.New(aerr.Malformed, "a tree lists the name "+e.Name+" twice")
		}
		seen[e.Name] = true
	}
	if sel.all {
		for _, e := range es {
			if err := w.entry(rel, e); err != nil {
				return err
			}
		}
		return nil
	}
	names := make([]string, 0, len(sel.children))
	for n := range sel.children {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		child := sel.children[n]
		e, ok := find(es, n)
		if !ok {
			return aerr.New(aerr.NotFound, "no such path in the commit: "+strings.Join(append(append([]string{}, rel...), n), "/"))
		}
		if e.Kind() != "tree" {
			if !child.all {
				return aerr.New(aerr.NotFound, "not a directory: "+strings.Join(append(append([]string{}, rel...), n), "/"))
			}
			if err := w.entry(rel, e); err != nil {
				return err
			}
			continue
		}
		if child.all {
			if err := w.entry(rel, e); err != nil {
				return err
			}
			continue
		}
		if err := w.mkdir(append(append([]string{}, rel...), n)); err != nil {
			return err
		}
		if err := w.tree(e.ID, append(append([]string{}, rel...), n), child); err != nil {
			return err
		}
	}
	return nil
}

func find(es []gittree.Entry, name string) (gittree.Entry, bool) {
	for _, e := range es {
		if e.Name == name {
			return e, true
		}
	}
	return gittree.Entry{}, false
}

func (w *writer) mkdir(rel []string) error {
	return fsError(w.r.MkdirAll(strings.Join(rel, "/"), 0o755))
}

func (w *writer) entry(dir []string, e gittree.Entry) error {
	if !SafeName(e.Name) {
		return aerr.New(aerr.NotPermitted, "the commit holds a name a checkout must not contain: "+e.Name)
	}
	rel := append(append([]string{}, dir...), e.Name)
	p := strings.Join(rel, "/")
	if len(p) > MaxPathLen {
		return aerr.New(aerr.BoundExceeded, "path is too long")
	}
	if len(dir) > 0 {
		if err := w.mkdir(dir); err != nil {
			return err
		}
	}
	switch e.Kind() {
	case "tree":
		if err := w.mkdir(rel); err != nil {
			return err
		}
		return w.tree(e.ID, rel, &node{all: true})
	case "commit":
		// A submodule: git leaves an empty directory.
		return w.mkdir(rel)
	}
	if w.res.Files >= w.lim.MaxFiles {
		return aerr.New(aerr.BoundExceeded, "too many files")
	}
	data, err := w.f.Blob(w.ctx, e.ID)
	if err != nil {
		return err
	}
	if w.res.Bytes+int64(len(data)) > w.lim.MaxBytes {
		return aerr.New(aerr.BoundExceeded, "checkout larger than the byte bound")
	}
	if e.Mode == "120000" {
		if err := checkLink(dir, string(data)); err != nil {
			return err
		}
		if err := w.r.Symlink(string(data), p); err != nil {
			return fsError(err)
		}
	} else {
		perm := os.FileMode(0o644)
		if e.Mode == "100755" {
			perm = 0o755
		}
		f, err := w.r.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return fsError(err)
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return fsError(err)
		}
		if err := f.Close(); err != nil {
			return fsError(err)
		}
	}
	w.res.Files++
	w.res.Bytes += int64(len(data))
	return nil
}

// checkLink refuses a symlink target that is empty, absolute, holds a NUL or a
// backslash, or leaves the checkout lexically (the Root refuses a link that
// would be followed out at run time too; this refuses the link itself).
func checkLink(dir []string, target string) error {
	bad := func() error {
		return aerr.New(aerr.NotPermitted, "a symbolic link in the commit points outside the checkout")
	}
	if target == "" || len(target) > MaxPathLen || strings.ContainsAny(target, "\\\x00") || strings.HasPrefix(target, "/") {
		return bad()
	}
	res := path.Clean(path.Join(append(append([]string{}, dir...), target)...))
	if res == ".." || strings.HasPrefix(res, "../") {
		return bad()
	}
	for _, e := range strings.Split(res, "/") {
		if strings.EqualFold(e, ".git") {
			return bad()
		}
	}
	return nil
}
