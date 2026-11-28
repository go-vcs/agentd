package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-vcs/agentd/internal/gittree"
	"github.com/go-vcs/agentd/internal/objcache"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

// slowBE answers blobs slowly and records how many requests were in flight at
// once, which is what the bulk path is for and what its bound limits.
type slowBE struct {
	*objBE
	cur, max atomic.Int64
}

func (s *slowBE) Object(ctx context.Context, repo, kind, id string) ([]byte, error) {
	if kind == objcache.Blob {
		n := s.cur.Add(1)
		defer s.cur.Add(-1)
		for {
			m := s.max.Load()
			if n <= m || s.max.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.objBE.Object(ctx, repo, kind, id)
}

// manyFiles stores a commit over n files f000..; returns the commit and the id
// of the first file's blob.
func manyFiles(t *testing.T, be *objBE, n int) (commit, firstBlob string) {
	t.Helper()
	es := make([]gittree.Entry, n)
	for i := range es {
		id := put(t, be, objcache.Blob, []byte(fmt.Sprintf("file %d\n", i)))
		if i == 0 {
			firstBlob = id
		}
		es[i] = gittree.Entry{Mode: "100644", Name: fmt.Sprintf("f%03d", i), ID: id}
	}
	raw, err := gittree.Build(es)
	if err != nil {
		t.Fatal(err)
	}
	tree := put(t, be, objcache.Tree, raw)
	return put(t, be, objcache.Commit, []byte("tree "+tree+"\nauthor A <a@x> 1 +0000\ncommitter A <a@x> 1 +0000\n\nm\n")), firstBlob
}

func bulk(t *testing.T, n, workers int) (*slowBE, string, string, func(wire.MaterializeReq) (int, []byte), string) {
	t.Helper()
	be := &slowBE{objBE: &objBE{fakeBE: fakeBE{state: verdict.Green}}}
	commit, first := manyFiles(t, be.objBE, n)
	root := t.TempDir()
	ts, _ := start(t, be, func(l *Limits) { l.MaterializeRoot = root; l.FetchWorkers = workers })
	sess := hello(t, ts, "a")
	return be, commit, first, func(r wire.MaterializeReq) (int, []byte) {
		r.Session, r.Repo = sess, "o/r"
		return post(t, ts, "/v1/materialize", r)
	}, root
}

func TestBulkFetchRunsBlobsConcurrentlyUnderTheBound(t *testing.T) {
	be, commit, _, do, root := bulk(t, 40, 4)
	code, body := do(wire.MaterializeReq{Commit: commit, Dir: "w"})
	var r wire.MaterializeReply
	if code != 200 || json.Unmarshal(body, &r) != nil || r.Files != 40 {
		t.Fatalf("%d %s", code, body)
	}
	if m := be.max.Load(); m < 2 || m > 4 {
		t.Fatalf("%d blob requests in flight at once, want 2..4", m)
	}
	// 1 commit + 1 tree + 40 blobs, each fetched once however many workers.
	if n := be.fetches.Load(); n != 42 {
		t.Fatalf("%d upstream fetches", n)
	}
	if b, err := os.ReadFile(filepath.Join(root, "w", "f039")); err != nil || string(b) != "file 39\n" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestBulkFetchWorkersAreCapped(t *testing.T) {
	be, commit, _, do, _ := bulk(t, 70, 100000)
	if code, body := do(wire.MaterializeReq{Commit: commit, Dir: "w"}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if m := be.max.Load(); m > MaxFetchWorkers {
		t.Fatalf("%d in flight, cap is %d", m, MaxFetchWorkers)
	}
}

func TestBulkFetchOfOneWorkerIsSequential(t *testing.T) {
	be, commit, _, do, _ := bulk(t, 12, 1)
	if code, body := do(wire.MaterializeReq{Commit: commit, Dir: "w"}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if m := be.max.Load(); m != 1 {
		t.Fatalf("%d in flight with one worker", m)
	}
}

func TestBulkFetchOfAMissingBlobIsReportedAndLeavesNothing(t *testing.T) {
	be, commit, first, do, root := bulk(t, 40, 4)
	delete(be.objects, objcache.Blob+":"+first)
	code, body := do(wire.MaterializeReq{Commit: commit, Dir: "w"})
	if code != 404 {
		t.Fatalf("%d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "w")); err == nil {
		t.Fatal("a failed checkout left its directory")
	}
}
