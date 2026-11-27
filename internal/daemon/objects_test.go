package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-vcs/agentd/internal/aerr"
	"github.com/go-vcs/agentd/internal/gittree"
	"github.com/go-vcs/agentd/internal/objcache"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

// objBE serves objects from a map keyed by id, counting fetches; lie makes it
// answer with bytes that do not hash to the id asked for.
type objBE struct {
	fakeBE
	mu2     sync.Mutex
	objects map[string][]byte
	fetches atomic.Int64
	lie     bool
	gate    chan struct{}
}

func (o *objBE) Object(_ context.Context, _, kind, id string) ([]byte, error) {
	o.fetches.Add(1)
	if o.gate != nil {
		<-o.gate
	}
	o.mu2.Lock()
	defer o.mu2.Unlock()
	d, ok := o.objects[kind+":"+id]
	if !ok {
		return nil, aerr.New(aerr.NotFound, "no such object")
	}
	if o.lie {
		return append([]byte("tampered "), d...), nil
	}
	return d, nil
}

func put(t *testing.T, o *objBE, kind string, data []byte) string {
	t.Helper()
	id, err := objcache.ID(40, kind, data)
	if err != nil {
		t.Fatal(err)
	}
	if o.objects == nil {
		o.objects = map[string][]byte{}
	}
	o.objects[kind+":"+id] = data
	return id
}

func readBlob(t *testing.T, ts *httptest.Server, sess, id string) (int, []byte) {
	t.Helper()
	return post(t, ts, "/v1/read-blob", wire.ObjectReq{Session: sess, Repo: "o/r", ID: id})
}

func TestReadBlobVerifiesCachesAndSharesAcrossSessions(t *testing.T) {
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}}
	id := put(t, be, objcache.Blob, []byte("hello\n"))
	ts, s := start(t, be, nil)
	a, b := hello(t, ts, "a"), hello(t, ts, "b")

	code, body := readBlob(t, ts, a, id)
	var r wire.BlobReply
	if code != 200 || json.Unmarshal(body, &r) != nil || r.Cached || r.Size != 6 {
		t.Fatalf("first read: %d %s", code, body)
	}
	if d, _ := base64.StdEncoding.DecodeString(r.Content); string(d) != "hello\n" {
		t.Fatalf("content %q", d)
	}
	// Another session: served from the cache, no second upstream fetch.
	code, body = readBlob(t, ts, b, id)
	if code != 200 || json.Unmarshal(body, &r) != nil || !r.Cached {
		t.Fatalf("second read: %d %s", code, body)
	}
	if be.fetches.Load() != 1 {
		t.Fatalf("%d upstream fetches for one object", be.fetches.Load())
	}
	if st := s.Cache().Stats(); st.Hits != 1 || st.Entries != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestPoisonedUpstreamIsRefusedAndNotKept(t *testing.T) {
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}, lie: true}
	id := put(t, be, objcache.Blob, []byte("hello\n"))
	ts, s := start(t, be, nil)
	a := hello(t, ts, "a")
	if code, b := readBlob(t, ts, a, id); code != 502 || reason(b) != aerr.ObjectMismatch {
		t.Fatalf("%d %s", code, b)
	}
	if st := s.Cache().Stats(); st.Entries != 0 || st.Refused != 1 {
		t.Fatalf("poison kept: %+v", st)
	}
	// The truth is served as soon as the upstream stops lying.
	be.lie = false
	if code, b := readBlob(t, ts, a, id); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
}

func TestReadTreeParsesAndRefusesWrongKind(t *testing.T) {
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}}
	blob := put(t, be, objcache.Blob, []byte("hello\n"))
	raw, _ := gittree.Build([]gittree.Entry{{Mode: "100644", Name: "a.txt", ID: blob}})
	tree := put(t, be, objcache.Tree, raw)
	ts, _ := start(t, be, nil)
	a := hello(t, ts, "a")
	code, b := post(t, ts, "/v1/read-tree", wire.ObjectReq{Session: a, Repo: "o/r", ID: tree})
	var r wire.TreeReply
	if code != 200 || json.Unmarshal(b, &r) != nil || len(r.Entries) != 1 || r.Entries[0].Name != "a.txt" || r.Entries[0].ID != blob {
		t.Fatalf("%d %s", code, b)
	}
	// The tree's id asked for as a blob: the cache knows it is a tree.
	if code, b := readBlob(t, ts, a, tree); code != 404 || reason(b) != aerr.NotFound {
		t.Fatalf("wrong kind: %d %s", code, b)
	}
}

// A tree the upstream does not have (or whose id it lies about) must be an
// error, never an empty tree: no bytes parse as zero entries.
func TestReadTreeUpstreamFailureIsNotAnEmptyTree(t *testing.T) {
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}}
	raw, _ := gittree.Build([]gittree.Entry{{Mode: "100644", Name: "a", ID: strings.Repeat("ab", 20)}})
	known := put(t, be, objcache.Tree, raw)
	ts, _ := start(t, be, nil)
	a := hello(t, ts, "a")
	missing := strings.Repeat("c", 40)
	if code, b := post(t, ts, "/v1/read-tree", wire.ObjectReq{Session: a, Repo: "o/r", ID: missing}); code != 404 || reason(b) != aerr.NotFound {
		t.Fatalf("missing tree: %d %s", code, b)
	}
	be.lie = true
	if code, b := post(t, ts, "/v1/read-tree", wire.ObjectReq{Session: a, Repo: "o/r", ID: known}); code != 502 || reason(b) != aerr.ObjectMismatch {
		t.Fatalf("poisoned tree: %d %s", code, b)
	}
}

func TestReadObjectInputRefusals(t *testing.T) {
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}}
	ts, _ := start(t, be, nil)
	a := hello(t, ts, "a")
	for name, req := range map[string]wire.ObjectReq{
		"abbreviated": {Session: a, Repo: "o/r", ID: "abc1234"},
		"bad repo":    {Session: a, Repo: "o/../r", ID: c1},
	} {
		code, b := post(t, ts, "/v1/read-blob", req)
		want := map[string]string{"abbreviated": aerr.AbbreviatedID, "bad repo": aerr.Malformed}[name]
		if code != 400 || reason(b) != want {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if be.fetches.Load() != 0 {
		t.Fatal("the upstream was reached")
	}
	if code, b := readBlob(t, ts, a, c1); code != 404 || reason(b) != aerr.NotFound {
		t.Fatalf("missing: %d %s", code, b)
	}
}

func TestBackendWithoutObjectsIsUnsupported(t *testing.T) {
	ts, _ := start(t, &fakeBE{state: verdict.Green}, nil)
	a := hello(t, ts, "a")
	if code, b := readBlob(t, ts, a, c1); code != 501 || reason(b) != aerr.UnsupportedBackend {
		t.Fatalf("%d %s", code, b)
	}
}

func TestTreeThatHashesRightButIsNotATreeIsRefused(t *testing.T) {
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}}
	id := put(t, be, objcache.Tree, []byte("not a tree"))
	ts, _ := start(t, be, nil)
	a := hello(t, ts, "a")
	if code, b := post(t, ts, "/v1/read-tree", wire.ObjectReq{Session: a, Repo: "o/r", ID: id}); code != 502 || reason(b) != aerr.ObjectMismatch {
		t.Fatalf("%d %s", code, b)
	}
}

func TestConcurrentMissesShareOneFetch(t *testing.T) {
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}, gate: make(chan struct{})}
	id := put(t, be, objcache.Blob, []byte("shared"))
	ts, s := start(t, be, nil)
	a := hello(t, ts, "a")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, b := readBlob(t, ts, a, id); code != 200 {
				t.Errorf("%d %s", code, b)
			}
		}()
	}
	for s.Cache().Stats().Fetches == 0 {
		// wait for the leader to be inside the upstream call
		if be.fetches.Load() > 0 {
			break
		}
	}
	close(be.gate)
	wg.Wait()
	if be.fetches.Load() != 1 {
		t.Fatalf("%d upstream fetches for 12 concurrent reads", be.fetches.Load())
	}
}

// A blob larger than the per-object bound is refused, and a flood of distinct
// objects stays inside the cache bounds, under a memory limit.
func TestObjectBombIsRefusedUnderAMemoryLimit(t *testing.T) {
	old := debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(old)
	be := &objBE{fakeBE: fakeBE{state: verdict.Green}}
	big := put(t, be, objcache.Blob, []byte(strings.Repeat("x", 3<<20)))
	ts, s := start(t, be, func(l *Limits) { l.Cache = objcache.Limits{MaxBytes: 1 << 20, MaxEntries: 8, MaxObject: 64 << 10} })
	a := hello(t, ts, "a")
	if code, b := readBlob(t, ts, a, big); code != 413 || reason(b) != aerr.BoundExceeded {
		t.Fatalf("%d %s", code, b)
	}
	for i := 0; i < 200; i++ {
		id := put(t, be, objcache.Blob, []byte(strings.Repeat("y", 40<<10)+string(rune('a'+i%26))+strings.Repeat("z", i)))
		if code, b := readBlob(t, ts, a, id); code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		if st := s.Cache().Stats(); st.Entries > 8 || st.Bytes > 1<<20 {
			t.Fatalf("bound broken: %+v", st)
		}
	}
}
