package materialize

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
)

type fake struct {
	trees     map[string][]gittree.Entry
	blobs     map[string][]byte
	err       error
	treeCalls int
	blobCalls int
}

func (f *fake) Tree(_ context.Context, id string) ([]gittree.Entry, error) {
	f.treeCalls++
	if f.err != nil {
		return nil, f.err
	}
	es, ok := f.trees[id]
	if !ok {
		return nil, aerr.New(aerr.NotFound, "no tree")
	}
	return es, nil
}

func (f *fake) Blob(_ context.Context, id string) ([]byte, error) {
	f.blobCalls++
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.blobs[id]
	if !ok {
		return nil, aerr.New(aerr.NotFound, "no blob")
	}
	return b, nil
}

// layout:
//
//	README.md  run.sh (exec)  lnk -> README.md  sub (submodule)
//	src/main.go  src/pkg/x.go
func sample() *fake {
	return &fake{
		trees: map[string][]gittree.Entry{
			"T0": {
				{Mode: "100644", Name: "README.md", ID: "b1"},
				{Mode: "100755", Name: "run.sh", ID: "b2"},
				{Mode: "120000", Name: "lnk", ID: "b3"},
				{Mode: "160000", Name: "sub", ID: "c1"},
				{Mode: "40000", Name: "src", ID: "T1"},
			},
			"T1": {{Mode: "100644", Name: "main.go", ID: "b4"}, {Mode: "40000", Name: "pkg", ID: "T2"}},
			"T2": {{Mode: "100644", Name: "x.go", ID: "b5"}},
		},
		blobs: map[string][]byte{"b1": []byte("hello\n"), "b2": []byte("#!/bin/sh\n"), "b3": []byte("README.md"), "b4": []byte("package main\n"), "b5": []byte("package pkg\n")},
	}
}

func open(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

func reason(err error) string { return aerr.As(err).Reason }

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestWholeTree(t *testing.T) {
	r, dir := open(t)
	res, err := Write(context.Background(), r, "out", "T0", nil, sample(), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 5 || res.Bytes != int64(6+10+9+13+12) {
		t.Fatalf("%+v", res)
	}
	out := filepath.Join(dir, "out")
	for p, want := range map[string]string{"README.md": "hello\n", "src/main.go": "package main\n", "src/pkg/x.go": "package pkg\n"} {
		if b, err := os.ReadFile(filepath.Join(out, p)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", p, b, err)
		}
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(out, "run.sh")); fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("run.sh is not executable: %v", fi.Mode())
		}
		if fi, _ := os.Stat(filepath.Join(out, "README.md")); fi.Mode().Perm()&0o111 != 0 {
			t.Errorf("README.md is executable: %v", fi.Mode())
		}
		if to, err := os.Readlink(filepath.Join(out, "lnk")); err != nil || to != "README.md" {
			t.Errorf("lnk: %q %v", to, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(out, "sub")); err != nil || !fi.IsDir() {
		t.Errorf("a submodule is an empty directory: %v %v", fi, err)
	}
	if es, _ := os.ReadDir(filepath.Join(out, "sub")); len(es) != 0 {
		t.Errorf("submodule directory is not empty")
	}
}

func TestSparsePaths(t *testing.T) {
	r, dir := open(t)
	f := sample()
	res, err := Write(context.Background(), r, "a/b/out", "T0", []string{"src/pkg/x.go", "README.md", "src/main.go"}, f, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 {
		t.Fatalf("%+v", res)
	}
	out := filepath.Join(dir, "a", "b", "out")
	for _, p := range []string{"README.md", "src/main.go", "src/pkg/x.go"} {
		if !exists(filepath.Join(out, p)) {
			t.Errorf("%s missing", p)
		}
	}
	for _, p := range []string{"run.sh", "lnk", "sub"} {
		if exists(filepath.Join(out, p)) {
			t.Errorf("%s written though not asked for", p)
		}
	}
	if f.blobCalls != 3 {
		t.Errorf("%d blobs fetched for 3 files", f.blobCalls)
	}
	// A directory path takes its whole subtree; a path under it adds nothing.
	r2, dir2 := open(t)
	res, err = Write(context.Background(), r2, "o", "T0", []string{"src", "src/pkg"}, sample(), DefaultLimits())
	if err != nil || res.Files != 2 || !exists(filepath.Join(dir2, "o", "src", "pkg", "x.go")) {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRefusalsLeaveNothingBehind(t *testing.T) {
	tooDeep := &fake{trees: map[string][]gittree.Entry{}, blobs: map[string][]byte{}}
	id := "T0"
	for i := 0; i < 40; i++ {
		next := "T" + strings.Repeat("n", i+1)
		tooDeep.trees[id] = []gittree.Entry{{Mode: "40000", Name: "d", ID: next}}
		id = next
	}
	tooDeep.trees[id] = nil

	mut := func(edit func(f *fake)) *fake { f := sample(); edit(f); return f }
	cases := []struct {
		name  string
		f     *fake
		paths []string
		lim   func(l *Limits)
		want  string
	}{
		{"missing path", sample(), []string{"nope"}, nil, aerr.NotFound},
		{"missing nested path", sample(), []string{"src/nope"}, nil, aerr.NotFound},
		{"through a file", sample(), []string{"README.md/x"}, nil, aerr.NotFound},
		{"git dir", sample(), []string{".git/config"}, nil, aerr.Malformed},
		{"git dir, other case", sample(), []string{".GIT"}, nil, aerr.Malformed},
		{"dotdot", sample(), []string{"../x"}, nil, aerr.Malformed},
		{"absolute", sample(), []string{"/etc"}, nil, aerr.Malformed},
		{"double slash", sample(), []string{"src//main.go"}, nil, aerr.Malformed},
		{"backslash", sample(), []string{`src\main.go`}, nil, aerr.Malformed},
		{"trailing slash", sample(), []string{"src/"}, nil, aerr.Malformed},
		{"empty path", sample(), []string{""}, nil, aerr.Malformed},
		{"too many paths", sample(), []string{"a", "b", "c"}, func(l *Limits) { l.MaxPaths = 2 }, aerr.BoundExceeded},
		{"path too deep", sample(), []string{"a/b/c"}, func(l *Limits) { l.MaxDepth = 2 }, aerr.BoundExceeded},
		{"too many files", sample(), nil, func(l *Limits) { l.MaxFiles = 4 }, aerr.BoundExceeded},
		{"too many bytes", sample(), nil, func(l *Limits) { l.MaxBytes = 40 }, aerr.BoundExceeded},
		{"too many trees", sample(), nil, func(l *Limits) { l.MaxTrees = 2 }, aerr.BoundExceeded},
		{"tree too deep", tooDeep, nil, nil, aerr.BoundExceeded},
		{"entry named .git", mut(func(f *fake) {
			f.trees["T0"] = append(f.trees["T0"], gittree.Entry{Mode: "100644", Name: ".git", ID: "b1"})
		}), nil, nil, aerr.NotPermitted},
		{"entry named GIT~1", mut(func(f *fake) {
			f.trees["T1"] = append(f.trees["T1"], gittree.Entry{Mode: "40000", Name: "GIT~1", ID: "T2"})
		}), nil, nil, aerr.NotPermitted},
		{"entry with a backslash", mut(func(f *fake) {
			f.trees["T2"] = append(f.trees["T2"], gittree.Entry{Mode: "100644", Name: `a\b`, ID: "b1"})
		}), nil, nil, aerr.NotPermitted},
		{"duplicate name", mut(func(f *fake) {
			f.trees["T1"] = append(f.trees["T1"], gittree.Entry{Mode: "100644", Name: "main.go", ID: "b1"})
		}), nil, nil, aerr.Malformed},
		{"absolute symlink", mut(func(f *fake) { f.blobs["b3"] = []byte("/etc/passwd") }), nil, nil, aerr.NotPermitted},
		{"escaping symlink", mut(func(f *fake) { f.blobs["b3"] = []byte("../x") }), nil, nil, aerr.NotPermitted},
		{"symlink escaping from a subdirectory", mut(func(f *fake) {
			f.trees["T1"] = append(f.trees["T1"], gittree.Entry{Mode: "120000", Name: "up", ID: "b6"})
			f.blobs["b6"] = []byte("../../x")
		}), nil, nil, aerr.NotPermitted},
		{"symlink into .git", mut(func(f *fake) { f.blobs["b3"] = []byte("src/../.git/config") }), nil, nil, aerr.NotPermitted},
		{"symlink with a backslash", mut(func(f *fake) { f.blobs["b3"] = []byte(`a\b`) }), nil, nil, aerr.NotPermitted},
		{"empty symlink", mut(func(f *fake) { f.blobs["b3"] = nil }), nil, nil, aerr.NotPermitted},
		{"missing blob", mut(func(f *fake) { delete(f.blobs, "b5") }), nil, nil, aerr.NotFound},
		{"upstream refused", mut(func(f *fake) { f.err = aerr.New(aerr.ObjectMismatch, "x") }), nil, nil, aerr.ObjectMismatch},
	}
	for _, tc := range cases {
		r, dir := open(t)
		lim := DefaultLimits()
		if tc.lim != nil {
			tc.lim(&lim)
		}
		_, err := Write(context.Background(), r, "out", "T0", tc.paths, tc.f, lim)
		if err == nil || reason(err) != tc.want {
			t.Errorf("%s: want %s, got %v", tc.name, tc.want, err)
		}
		if exists(filepath.Join(dir, "out")) {
			t.Errorf("%s: the destination was left behind", tc.name)
		}
	}
}

func TestDestinationRefusals(t *testing.T) {
	r, dir := open(t)
	if err := os.Mkdir(filepath.Join(dir, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "taken", "keep"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(context.Background(), r, "taken", "T0", nil, sample(), DefaultLimits()); reason(err) != aerr.Conflict {
		t.Fatalf("existing destination: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "taken", "keep")); string(b) != "mine" {
		t.Fatal("an existing directory was touched, or removed, by a refused materialization")
	}
	for _, d := range []string{"", "..", "../x", "/abs", "a//b", ".git", "a/.GIT/b", `a\b`, "a/", strings.Repeat("a/", MaxDirDepth) + "a", strings.Repeat("n", MaxNameLen+1)} {
		if _, err := Write(context.Background(), r, d, "T0", nil, sample(), DefaultLimits()); err == nil {
			t.Errorf("destination %q accepted", d)
		}
	}
	// A destination below a plain file cannot be created.
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(context.Background(), r, "file/out", "T0", nil, sample(), DefaultLimits()); err == nil {
		t.Error("destination under a file accepted")
	}
}

// A symbolic link planted in the root that points outside it must not carry a
// write out: the Root refuses to follow it.
func TestSymlinkedParentCannotLeadOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege on Windows")
	}
	r, dir := open(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(context.Background(), r, "escape/out", "T0", nil, sample(), DefaultLimits()); err == nil {
		t.Fatal("wrote through a symlink that leaves the root")
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Fatalf("something was written outside the root: %v", es)
	}
}

func TestCancelledContextStopsAndCleans(t *testing.T) {
	r, dir := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := sample()
	f.err = ctx.Err()
	if _, err := Write(ctx, r, "out", "T0", nil, f, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if exists(filepath.Join(dir, "out")) {
		t.Fatal("left behind")
	}
}

func TestFsErrorMapping(t *testing.T) {
	if reason(fsError(fs.ErrExist)) != aerr.Conflict {
		t.Error("an existing path is a conflict")
	}
	if reason(fsError(errors.New("disk on fire"))) != aerr.Internal {
		t.Error("anything else is internal, without its detail")
	}
}

func TestNamesAndElems(t *testing.T) {
	for _, n := range []string{"a", "a.b", ".gitignore", ".github", "git", "x y", "é"} {
		if !SafeName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", ".", "..", ".git", ".GIT", ".Git", "git~1", "GIT~1", "a/b", `a\b`, "a\x00", strings.Repeat("n", MaxNameLen+1)} {
		if SafeName(n) {
			t.Errorf("%q accepted", n)
		}
	}
	if es, err := Elems("a/b/c", 3); err != nil || len(es) != 3 {
		t.Errorf("%v %v", es, err)
	}
	if _, err := Elems("a/b/c", 2); reason(err) != aerr.BoundExceeded {
		t.Errorf("depth: %v", err)
	}
	if _, err := Elems(strings.Repeat("a", MaxPathLen+1), 3); reason(err) != aerr.Malformed {
		t.Errorf("length: %v", err)
	}
}

func FuzzElems(f *testing.F) {
	for _, s := range []string{"a/b", "../x", "/abs", "a//b", ".git", "a/.GIT/b", "x\x00y", `a\b`, ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		es, err := Elems(p, 8)
		if err != nil {
			return
		}
		if len(es) == 0 || len(es) > 8 || strings.Join(es, "/") != p {
			t.Fatalf("%q split into %q", p, es)
		}
		for _, e := range es {
			if !SafeName(e) {
				t.Fatalf("%q accepted with the element %q", p, e)
			}
		}
	})
}

// failing makes the nth filesystem or fetch operation fail.
type failing struct {
	inner fsRoot
	n     *int // operations left before the failing one
	hit   *bool
}

var errInjected = errors.New("injected")

func (f failing) tick() error {
	if *f.n == 0 {
		*f.hit = true
		*f.n = -1
		return errInjected
	}
	if *f.n > 0 {
		*f.n--
	}
	return nil
}

func (f failing) MkdirAll(name string, perm os.FileMode) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.inner.MkdirAll(name, perm)
}

func (f failing) Mkdir(name string, perm os.FileMode) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.inner.Mkdir(name, perm)
}

func (f failing) Symlink(o, n string) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.inner.Symlink(o, n)
}

func (f failing) RemoveAll(name string) error { return f.inner.RemoveAll(name) }
func (f failing) Close() error                { return f.inner.Close() }

func (f failing) OpenRoot(name string) (fsRoot, error) {
	if err := f.tick(); err != nil {
		// A usable root with the error: code that ignored the error would carry on.
		r, _ := f.inner.OpenRoot(name)
		return failing{r, f.n, f.hit}, err
	}
	r, err := f.inner.OpenRoot(name)
	return failing{r, f.n, f.hit}, err
}

type failingFile struct {
	failing
	w io.WriteCloser
}

func (f failingFile) Write(p []byte) (int, error) {
	if err := f.tick(); err != nil {
		return 0, err
	}
	return f.w.Write(p)
}

func (f failingFile) Close() error {
	err := f.w.Close()
	if e := f.tick(); e != nil {
		return e
	}
	return err
}

func (f failing) OpenFile(name string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	if err := f.tick(); err != nil {
		w, _ := f.inner.OpenFile(name, flag, perm) // usable, so that ignoring the error would carry on
		return w, err
	}
	w, err := f.inner.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return failingFile{f, w}, nil
}

type failingFetch struct {
	*fake
	f failing
}

func (c failingFetch) Tree(ctx context.Context, id string) ([]gittree.Entry, error) {
	if err := c.f.tick(); err != nil {
		return nil, err
	}
	return c.fake.Tree(ctx, id)
}

func (c failingFetch) Blob(ctx context.Context, id string) ([]byte, error) {
	if err := c.f.tick(); err != nil {
		return nil, err
	}
	return c.fake.Blob(ctx, id)
}

// Whichever single operation fails, the error is the one returned and nothing
// of the checkout is left; with no failure the same run succeeds.
func TestAnyOneOperationFailingLeavesNothingAndIsReported(t *testing.T) {
	selections := [][]string{nil, {"README.md", "src/pkg", "src/main.go", "lnk", "sub"}}
	for _, paths := range selections {
		ops := 0
		for n := 0; ; n++ {
			r, dir := open(t)
			left, hit := n, false
			fl := failing{osRoot{r}, &left, &hit}
			_, err := write(context.Background(), fl, "a/out", "T0", paths, failingFetch{sample(), fl}, DefaultLimits())
			if !hit {
				if err != nil {
					t.Fatalf("paths %v, no injected failure yet: %v", paths, err)
				}
				ops = n
				break
			}
			if !errors.Is(err, errInjected) {
				t.Fatalf("paths %v, failure %d: %v", paths, n, err)
			}
			if exists(filepath.Join(dir, "a", "out")) {
				t.Fatalf("paths %v, failure %d left the checkout behind", paths, n)
			}
		}
		if ops < 10 {
			t.Fatalf("only %d operations counted for %v", ops, paths)
		}
	}
}

func TestPathLongerThanTheBoundIsRefused(t *testing.T) {
	// A chain of 20 directories with 250-byte names is deeper in bytes than
	// MaxPathLen although each name and the depth are within their bounds.
	f := &fake{trees: map[string][]gittree.Entry{}, blobs: map[string][]byte{"b": nil}}
	id := "T0"
	for i := 0; i < 20; i++ {
		next := "T" + strings.Repeat("n", i+1)
		f.trees[id] = []gittree.Entry{{Mode: "40000", Name: strings.Repeat(string(rune('a'+i)), 250), ID: next}}
		id = next
	}
	f.trees[id] = []gittree.Entry{{Mode: "100644", Name: "f", ID: "b"}}
	r, dir := open(t)
	if _, err := Write(context.Background(), r, "out", "T0", nil, f, DefaultLimits()); reason(err) != aerr.BoundExceeded {
		t.Fatalf("%v", err)
	}
	if exists(filepath.Join(dir, "out")) {
		t.Fatal("left behind")
	}
	long := strings.Repeat("a/", MaxPathLen) + "a"
	if _, err := Elems(long, 1<<20); reason(err) != aerr.Malformed {
		t.Fatalf("long path: %v", err)
	}
}
