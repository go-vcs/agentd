package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
	"github.com/go-vcs/agentd/internal/objcache"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

// repoOf stores a commit over a tree: README.md, src/main.go; returns the ids.
func repoOf(t *testing.T, be *objBE) (commit, tree string) {
	t.Helper()
	readme := put(t, be, objcache.Blob, []byte("hello\n"))
	main := put(t, be, objcache.Blob, []byte("package main\n"))
	src, _ := gittree.Build([]gittree.Entry{{Mode: "100644", Name: "main.go", ID: main}})
	srcID := put(t, be, objcache.Tree, src)
	raw, _ := gittree.Build([]gittree.Entry{{Mode: "100644", Name: "README.md", ID: readme}, {Mode: "40000", Name: "src", ID: srcID}})
	tree = put(t, be, objcache.Tree, raw)
	commit = put(t, be, objcache.Commit, []byte("tree "+tree+"\nauthor A <a@x> 1 +0000\ncommitter A <a@x> 1 +0000\n\nm\n"))
	return commit, tree
}

func mat(t *testing.T, root string, mod func(*Limits)) (*objBE, string, string, string, func(wire.MaterializeReq) (int, []byte)) {
	t.Helper()
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}}
	commit, tree := repoOf(t, be)
	ts, _ := start(t, be, func(l *Limits) {
		l.MaterializeRoot = root
		if mod != nil {
			mod(l)
		}
	})
	sess := hello(t, ts, "a")
	return be, commit, tree, sess, func(r wire.MaterializeReq) (int, []byte) {
		r.Session, r.Repo = sess, "o/r"
		return post(t, ts, "/v1/materialize", r)
	}
}

func TestMaterializeWritesTheVerifiedTree(t *testing.T) {
	root := t.TempDir()
	be, commit, tree, _, do := mat(t, root, nil)
	code, body := do(wire.MaterializeReq{Commit: commit, Dir: "work/a"})
	var r wire.MaterializeReply
	if code != 200 || json.Unmarshal(body, &r) != nil || r.Files != 2 || r.Tree != tree || r.Commit != commit || r.Dir != filepath.Join(root, "work", "a") {
		t.Fatalf("%d %s", code, body)
	}
	if b, err := os.ReadFile(filepath.Join(r.Dir, "src", "main.go")); err != nil || string(b) != "package main\n" {
		t.Fatalf("%q %v", b, err)
	}
	// 1 commit + 2 trees + 2 blobs, each fetched once.
	if n := be.fetches.Load(); n != 5 {
		t.Fatalf("%d upstream fetches", n)
	}
	// A second checkout of the same commit costs no upstream fetch at all.
	if code, body := do(wire.MaterializeReq{Commit: commit, Dir: "work/b", Paths: []string{"src/main.go"}}); code != 200 || be.fetches.Load() != 5 {
		t.Fatalf("%d %s fetches=%d", code, body, be.fetches.Load())
	}
	if _, err := os.Stat(filepath.Join(root, "work", "b", "README.md")); err == nil {
		t.Fatal("sparse checkout wrote a file not asked for")
	}
}

func TestMaterializeRefusals(t *testing.T) {
	root := t.TempDir()
	be, commit, _, _, do := mat(t, root, nil)
	if err := os.Mkdir(filepath.Join(root, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A commit with no tree header, and one that names a malformed tree id.
	noTree := put(t, be, objcache.Commit, []byte("parent x\n\nm\n"))
	badTree := put(t, be, objcache.Commit, []byte("tree "+strings.Repeat("Z", 40)+"\n\nm\n"))
	tooShort := put(t, be, objcache.Commit, []byte("tree abc"))
	cases := []struct {
		name string
		req  wire.MaterializeReq
		code int
		want string
	}{
		{"existing dir", wire.MaterializeReq{Commit: commit, Dir: "taken"}, 412, aerr.Conflict},
		{"dotdot", wire.MaterializeReq{Commit: commit, Dir: "../x"}, 400, aerr.Malformed},
		{"absolute", wire.MaterializeReq{Commit: commit, Dir: "/tmp/x"}, 400, aerr.Malformed},
		{"abbreviated commit", wire.MaterializeReq{Commit: commit[:10], Dir: "d"}, 400, aerr.AbbreviatedID},
		{"missing path", wire.MaterializeReq{Commit: commit, Dir: "d2", Paths: []string{"nope"}}, 404, aerr.NotFound},
		{"commit without tree", wire.MaterializeReq{Commit: noTree, Dir: "d3"}, 502, aerr.ObjectMismatch},
		{"commit with a bad tree id", wire.MaterializeReq{Commit: badTree, Dir: "d4"}, 502, aerr.ObjectMismatch},
		{"commit too short", wire.MaterializeReq{Commit: tooShort, Dir: "d5"}, 502, aerr.ObjectMismatch},
		{"unknown commit", wire.MaterializeReq{Commit: strings.Repeat("e", 40), Dir: "d6"}, 404, aerr.NotFound},
	}
	for _, tc := range cases {
		code, body := do(tc.req)
		if code != tc.code || reason(body) != tc.want {
			t.Errorf("%s: %d %s", tc.name, code, body)
		}
	}
	for _, d := range []string{"d", "d2", "d3", "d4", "d5", "d6"} {
		if _, err := os.Stat(filepath.Join(root, d)); err == nil {
			t.Errorf("%s was left behind by a refusal", d)
		}
	}
}

func TestMaterializeDisabledAndUnknownSession(t *testing.T) {
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}}
	commit, _ := repoOf(t, be)
	ts, _ := start(t, be, nil)
	a := hello(t, ts, "a")
	if code, b := post(t, ts, "/v1/materialize", wire.MaterializeReq{Session: a, Repo: "o/r", Commit: commit, Dir: "d"}); code != 403 || reason(b) != aerr.NotPermitted {
		t.Fatalf("disabled: %d %s", code, b)
	}
	if code, b := post(t, ts, "/v1/materialize", wire.MaterializeReq{Session: "s-" + strings.Repeat("0", 32), Repo: "o/r", Commit: commit, Dir: "d"}); code != 404 || reason(b) != aerr.UnknownSession {
		t.Fatalf("unknown session: %d %s", code, b)
	}
}

// Concurrent materializations are capped, and the refusal is retryable.
func TestMaterializeConcurrencyCap(t *testing.T) {
	root := t.TempDir()
	be, commit, _, _, do := mat(t, root, func(l *Limits) { l.MaterializeMax = 2 })
	be.gate = make(chan struct{})
	var wg sync.WaitGroup
	for _, d := range []string{"c1", "c2"} {
		wg.Add(1)
		go func() { defer wg.Done(); do(wire.MaterializeReq{Commit: commit, Dir: d}) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for be.fetches.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	code, body := do(wire.MaterializeReq{Commit: commit, Dir: "c3"})
	if code != 429 || reason(body) != aerr.RateLimited {
		t.Fatalf("third materialization: %d %s", code, body)
	}
	close(be.gate)
	wg.Wait()
	// The slot is free again.
	if code, body := do(wire.MaterializeReq{Commit: commit, Dir: "c4"}); code != 200 {
		t.Fatalf("after: %d %s", code, body)
	}
}

// A checkout bigger than its bounds is refused under a memory limit, and its
// partial output is removed.
func TestMaterializeBombIsRefusedUnderAMemoryLimit(t *testing.T) {
	old := debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(old)
	root := t.TempDir()
	be, commit, _, _, do := mat(t, root, func(l *Limits) { l.Materialize.MaxBytes = 10 })
	_ = be
	code, body := do(wire.MaterializeReq{Commit: commit, Dir: "bomb"})
	if code != 413 && code != 422 && code != 400 {
		t.Fatalf("%d %s", code, body)
	}
	if reason(body) != aerr.BoundExceeded {
		t.Fatalf("%s", body)
	}
	if _, err := os.Stat(filepath.Join(root, "bomb")); err == nil {
		t.Fatal("partial checkout left behind")
	}
}

func TestMaterializeTimeoutCleansUp(t *testing.T) {
	root := t.TempDir()
	be, commit, _, _, do := mat(t, root, func(l *Limits) { l.MaterializeTime = 50 * time.Millisecond })
	be.gate = make(chan struct{})
	defer close(be.gate)
	code, body := do(wire.MaterializeReq{Commit: commit, Dir: "slow"})
	if code == 200 {
		t.Fatalf("%d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "slow")); err == nil {
		t.Fatal("left behind")
	}
}
