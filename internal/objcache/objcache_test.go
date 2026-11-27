package objcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func blobID(t testing.TB, idLen int, data []byte) string {
	t.Helper()
	id, err := ID(idLen, Blob, data)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The first test of the file, so that a Get that mishandles a miss fails here
// with a message and not by hanging a later test.
func TestGetMissAndFetchStats(t *testing.T) {
	c := New(Limits{})
	if k, d, ok := c.Get(strings.Repeat("0", 40)); ok || k != "" || d != nil {
		t.Fatalf("miss returned %q %v %v", k, d, ok)
	}
	if s := c.Stats(); s.Misses != 1 || s.Hits != 0 {
		t.Fatalf("after a miss: %+v", s)
	}
	data := []byte("fetched once")
	id := blobID(t, 40, data)
	calls := 0
	fetch := func(context.Context) (string, []byte, error) { calls++; return Blob, data, nil }
	for i := 0; i < 3; i++ {
		if _, d, err := c.Fetch(context.Background(), id, fetch); err != nil || !bytes.Equal(d, data) {
			t.Fatal("fetch", err)
		}
	}
	if s := c.Stats(); calls != 1 || s.Fetches != 1 || s.Misses != 2 || s.Hits != 2 {
		t.Fatalf("calls %d, stats %+v: want 1 fetch, 2 misses, 2 hits", calls, s)
	}
}

// git hash-object of "hello\n" and of the empty blob, computed by git itself.
func TestIDMatchesGit(t *testing.T) {
	if got := blobID(t, 40, []byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("sha1 blob: %s", got)
	}
	if got := blobID(t, 40, nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Fatalf("empty blob: %s", got)
	}
	if got := blobID(t, 64, nil); got != "473a0f4c3be8a93681a267e3b1e9a7dcda1185436fe141f7749120a303721813" {
		t.Fatalf("sha256 empty blob: %s", got)
	}
	if _, err := ID(40, "tag", nil); !errors.Is(err, ErrKind) {
		t.Fatal(err)
	}
	if _, err := ID(41, Blob, nil); !errors.Is(err, ErrID) {
		t.Fatal(err)
	}
}

func TestAdmitVerifies(t *testing.T) {
	c := New(Limits{})
	data := []byte("content")
	id := blobID(t, 40, data)
	if err := c.Admit(id, Blob, data); err != nil {
		t.Fatal(err)
	}
	// Poisoning attempts: wrong content, wrong kind, wrong id length, bad kind.
	if err := c.Admit(id, Blob, []byte("other")); !errors.Is(err, ErrMismatch) {
		t.Fatalf("poison: %v", err)
	}
	if err := c.Admit(id, Tree, data); !errors.Is(err, ErrMismatch) {
		t.Fatalf("kind swap: %v", err)
	}
	if err := c.Admit(id[:39], Blob, data); !errors.Is(err, ErrID) {
		t.Fatalf("short id: %v", err)
	}
	if err := c.Admit(id, "tag", data); !errors.Is(err, ErrKind) {
		t.Fatalf("kind: %v", err)
	}
	other := blobID(t, 40, []byte("other"))
	if _, _, ok := c.Get(other); ok {
		t.Fatal("poisoned id is present")
	}
	k, d, ok := c.Get(id)
	if !ok || k != Blob || !bytes.Equal(d, data) {
		t.Fatal("clean object lost")
	}
	if s := c.Stats(); s.Refused != 4 || s.Entries != 1 || s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestAdmitCopies(t *testing.T) {
	c := New(Limits{})
	data := []byte("abc")
	id := blobID(t, 40, data)
	_ = c.Admit(id, Blob, data)
	data[0] = 'X'
	if _, d, _ := c.Get(id); string(d) != "abc" {
		t.Fatal("cache aliases the caller's buffer")
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c := New(Limits{MaxBytes: 3 * (10 + entryOverhead), MaxEntries: 100, MaxObject: 10})
	var ids [4]string
	for i := range ids {
		d := []byte(fmt.Sprintf("object-%02d!", i)[:10])
		ids[i] = blobID(t, 40, d)
		if i == 3 {
			// Touch 0 so that 1 is the least recently used.
			c.Get(ids[0])
		}
		if err := c.Admit(ids[i], Blob, d); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok := c.Get(ids[1]); ok {
		t.Fatal("least recently used survived")
	}
	for _, i := range []int{0, 2, 3} {
		if _, _, ok := c.Get(ids[i]); !ok {
			t.Fatalf("object %d evicted", i)
		}
	}
	if s := c.Stats(); s.Evictions != 1 || s.Entries != 3 {
		t.Fatalf("stats %+v", s)
	}
}

func TestEntryCountBound(t *testing.T) {
	c := New(Limits{MaxEntries: 5})
	for i := 0; i < 50; i++ {
		d := []byte{byte(i)}
		_ = c.Admit(blobID(t, 40, d), Blob, d)
	}
	if s := c.Stats(); s.Entries != 5 || s.Evictions != 45 {
		t.Fatalf("stats %+v", s)
	}
}

func TestReadmitDoesNotDuplicate(t *testing.T) {
	c := New(Limits{})
	d := []byte("x")
	id := blobID(t, 40, d)
	_ = c.Admit(id, Blob, d)
	_ = c.Admit(id, Blob, d)
	if s := c.Stats(); s.Entries != 1 || s.Bytes != cost(1) {
		t.Fatalf("stats %+v", s)
	}
}

func TestFetchSharesOneFetchAndVerifies(t *testing.T) {
	c := New(Limits{})
	data := []byte("shared")
	id := blobID(t, 40, data)
	var n atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) (string, []byte, error) {
		n.Add(1)
		<-release
		return Blob, data, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, d, err := c.Fetch(context.Background(), id, fetch); err != nil || !bytes.Equal(d, data) {
				t.Error("fetch failed", err)
			}
		}()
	}
	for c.Stats().Fetches == 0 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	// A late caller may arrive after the leader finished and hit the cache; none
	// may have fetched again.
	if _, _, err := c.Fetch(context.Background(), id, fetch); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 {
		t.Fatalf("%d upstream fetches for one id", n.Load())
	}
}

func TestFetchRefusesPoisonedAnswer(t *testing.T) {
	c := New(Limits{})
	id := blobID(t, 40, []byte("good"))
	_, d, err := c.Fetch(context.Background(), id, func(context.Context) (string, []byte, error) {
		return Blob, []byte("evil"), nil
	})
	if !errors.Is(err, ErrMismatch) || d != nil {
		t.Fatalf("poisoned answer returned: %q %v", d, err)
	}
	if _, _, ok := c.Get(id); ok {
		t.Fatal("poisoned answer cached")
	}
	// The failure is not remembered: a good answer works afterwards.
	if _, _, err := c.Fetch(context.Background(), id, func(context.Context) (string, []byte, error) {
		return Blob, []byte("good"), nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFetchErrorAndCancelledFollower(t *testing.T) {
	c := New(Limits{})
	id := blobID(t, 40, []byte("z"))
	boom := errors.New("boom")
	if _, _, err := c.Fetch(context.Background(), id, func(context.Context) (string, []byte, error) { return "", nil, boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A follower whose context ends while the leader is still fetching returns.
	started, release := make(chan struct{}), make(chan struct{})
	go c.Fetch(context.Background(), id, func(context.Context) (string, []byte, error) {
		close(started)
		<-release
		return Blob, []byte("z"), nil
	})
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Fetch(ctx, id, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
}

func TestZeroLimitsGetDefaults(t *testing.T) {
	c := New(Limits{})
	if c.lim != DefaultLimits() {
		t.Fatalf("%+v", c.lim)
	}
}

// TestBomb: a flood of distinct objects and an oversize object, under a memory
// limit. The cache must refuse the oversize one and stay inside its bounds.
func TestBomb(t *testing.T) {
	old := debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(old)
	lim := Limits{MaxBytes: 8 << 20, MaxEntries: 1000, MaxObject: 1 << 20}
	c := New(lim)

	huge := bytes.Repeat([]byte{'x'}, 64<<20)
	if err := c.Admit(blobID(t, 40, huge), Blob, huge); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a 64 MiB object under a 1 MiB bound: %v", err)
	}
	// Larger than the whole cache but under MaxObject is refused too.
	small := New(Limits{MaxBytes: 1 << 10, MaxEntries: 10, MaxObject: 1 << 20})
	mid := bytes.Repeat([]byte{'y'}, 2<<10)
	if err := small.Admit(blobID(t, 40, mid), Blob, mid); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an object larger than the cache: %v", err)
	}

	for i := 0; i < 200000; i++ {
		d := []byte(strings.Repeat("a", 900) + fmt.Sprint(i))
		if err := c.Admit(blobID(t, 40, d), Blob, d); err != nil {
			t.Fatal(err)
		}
		if s := c.Stats(); s.Bytes > lim.MaxBytes || s.Entries > lim.MaxEntries {
			t.Fatalf("bound broken at %d: %+v", i, s)
		}
	}
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > 192<<20 {
		t.Fatalf("heap %d MiB after the flood", m.HeapAlloc>>20)
	}
}

func BenchmarkGetHit(b *testing.B) {
	c := New(Limits{})
	d := bytes.Repeat([]byte{'q'}, 4096)
	id := blobID(b, 40, d)
	_ = c.Admit(id, Blob, d)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok := c.Get(id); !ok {
			b.Fatal("miss")
		}
	}
}

func TestGetHitAllocsExactly0(t *testing.T) {
	c := New(Limits{})
	d := []byte("alloc")
	id := blobID(t, 40, d)
	_ = c.Admit(id, Blob, d)
	if n := testing.AllocsPerRun(100, func() { c.Get(id) }); n != 0 {
		t.Fatalf("a cache hit allocates %v times", n)
	}
}
