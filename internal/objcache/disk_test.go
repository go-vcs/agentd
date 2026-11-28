package objcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

func openDisk(t *testing.T, mod func(*DiskLimits)) (*Disk, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	l := DefaultDiskLimits()
	if mod != nil {
		mod(&l)
	}
	d, err := OpenDisk(dir, l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, dir
}

func idOf(t *testing.T, kind string, data []byte) string {
	t.Helper()
	id, err := ID(40, kind, data)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDiskRoundTripModesAndPersistence(t *testing.T) {
	d, dir := openDisk(t, nil)
	data := []byte("hello\n")
	id := idOf(t, Blob, data)
	if err := d.Put(id, Blob, data); err != nil {
		t.Fatal(err)
	}
	k, got, ok := d.Get(id)
	if !ok || k != Blob || string(got) != "hello\n" {
		t.Fatalf("%q %q %v", k, got, ok)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(dir, id[:2], id)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%v %v", fi, err)
		}
		if fi, err := os.Stat(filepath.Join(dir, id[:2])); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%v %v", fi, err)
		}
	}
	// A new process finds it.
	d.Close()
	d2, err := OpenDisk(dir, DefaultDiskLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if k, got, ok := d2.Get(id); !ok || k != Blob || string(got) != "hello\n" {
		t.Fatalf("after reopen: %q %q %v", k, got, ok)
	}
	if s := d2.Stats(); s.Entries != 1 || s.Hits != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestDiskRefusesWhatItWouldNotVerify(t *testing.T) {
	d, _ := openDisk(t, func(l *DiskLimits) { l.MaxObject = 16 })
	data := []byte("x")
	id := idOf(t, Blob, data)
	cases := []struct {
		name, id, kind string
		data           []byte
		want           error
	}{
		{"wrong content", id, Blob, []byte("y"), ErrMismatch},
		{"wrong kind", id, Tree, data, ErrMismatch},
		{"unknown kind", id, "tag", data, ErrKind},
		{"short id", id[:39], Blob, data, ErrID},
		{"upper case id", strings.ToUpper(id), Blob, data, ErrID},
		{"path in id", "../" + id[3:], Blob, data, ErrID},
		{"too large", idOf(t, Blob, make([]byte, 17)), Blob, make([]byte, 17), ErrTooLarge},
	}
	for _, c := range cases {
		if err := d.Put(c.id, c.kind, c.data); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if s := d.Stats(); s.Entries != 0 || s.Refused != int64(len(cases)) {
		t.Fatalf("%+v", s)
	}
	if _, _, ok := d.Get("../../etc/passwd"); ok {
		t.Fatal("a path was read")
	}
}

func TestDiskDamagedFileIsAMissAndIsRemoved(t *testing.T) {
	data := []byte("hello\n")
	for name, damage := range map[string]func(p string) error{
		"flipped byte": func(p string) error { return os.WriteFile(p, []byte("blob\nhellO\n"), 0o600) },
		"truncated":    func(p string) error { return os.Truncate(p, 3) },
		"empty":        func(p string) error { return os.Truncate(p, 0) },
		"wrong kind":   func(p string) error { return os.WriteFile(p, []byte("tree\nhello\n"), 0o600) },
		"no header":    func(p string) error { return os.WriteFile(p, []byte("hello\n"), 0o600) },
		"swapped for a directory": func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.Mkdir(p, 0o700)
		},
		"swapped for a link": func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.Symlink("../../outside", p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, dir := openDisk(t, nil)
			id := idOf(t, Blob, data)
			if err := d.Put(id, Blob, data); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, id[:2], id)
			if err := damage(p); err != nil {
				t.Skip(err)
			}
			if _, _, ok := d.Get(id); ok {
				t.Fatal("a damaged file was returned")
			}
			if s := d.Stats(); s.Corrupt != 1 || s.Entries != 0 || s.Bytes != 0 {
				t.Fatalf("%+v", s)
			}
			if _, err := os.Lstat(p); err == nil {
				t.Fatal("the damaged file was left")
			}
			// And it can be stored again.
			if err := d.Put(id, Blob, data); err != nil {
				t.Fatal(err)
			}
			if _, _, ok := d.Get(id); !ok {
				t.Fatal("not stored again")
			}
		})
	}
}

func TestDiskBoundsEvictOldestFirstAndRemoveTheFiles(t *testing.T) {
	d, dir := openDisk(t, func(l *DiskLimits) { l.MaxEntries = 3 })
	var ids []string
	for i := 0; i < 5; i++ {
		data := []byte(fmt.Sprintf("object %d", i))
		id := idOf(t, Blob, data)
		ids = append(ids, id)
		if err := d.Put(id, Blob, data); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			d.Get(ids[0]) // 0 is now more recent than 1
		}
	}
	s := d.Stats()
	if s.Entries != 3 || s.Evicted != 2 {
		t.Fatalf("%+v", s)
	}
	for i, want := range []bool{true, false, false, true, true} {
		_, err := os.Lstat(filepath.Join(dir, ids[i][:2], ids[i]))
		if (err == nil) != want {
			t.Errorf("object %d on disk = %v, want %v", i, err == nil, want)
		}
	}

	d2, _ := openDisk(t, func(l *DiskLimits) { l.MaxBytes = 100 })
	for i := 0; i < 20; i++ {
		data := []byte(fmt.Sprintf("object %02d", i))
		if err := d2.Put(idOf(t, Blob, data), Blob, data); err != nil {
			t.Fatal(err)
		}
	}
	if s := d2.Stats(); s.Bytes > 100 || s.Entries == 0 {
		t.Fatalf("%+v", s)
	}
}

func TestDiskOpenTrimsToTheBoundsAndDropsStrays(t *testing.T) {
	d, dir := openDisk(t, nil)
	var ids []string
	for i := 0; i < 6; i++ {
		data := []byte(fmt.Sprintf("object %d", i))
		id := idOf(t, Blob, data)
		ids = append(ids, id)
		if err := d.Put(id, Blob, data); err != nil {
			t.Fatal(err)
		}
		// Older objects get older mtimes.
		old := time.Now().Add(time.Duration(i-10) * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, id[:2], id), old, old); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	// Strays: a leftover temp file, a stray file, a stray directory, a link.
	sub := filepath.Join(dir, ids[0][:2])
	for _, p := range []string{filepath.Join(sub, ".tmp-0011"), filepath.Join(dir, "README"), filepath.Join(dir, "zz")} {
		if strings.HasSuffix(p, "zz") {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(sub, strings.Repeat("a", 40))
	if os.Symlink(ids[1], link) != nil {
		link = ""
	}
	small := DefaultDiskLimits()
	small.MaxEntries = 2
	d2, err := OpenDisk(dir, small)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if s := d2.Stats(); s.Entries != 2 {
		t.Fatalf("%+v", s)
	}
	for _, id := range ids[:4] {
		if _, _, ok := d2.Get(id); ok {
			t.Fatalf("%s (old) survived the trim", id)
		}
	}
	for _, id := range ids[4:] {
		if _, _, ok := d2.Get(id); !ok {
			t.Fatalf("%s (recent) was evicted", id)
		}
	}
	for _, p := range []string{filepath.Join(sub, ".tmp-0011"), filepath.Join(dir, "README"), filepath.Join(dir, "zz"), link} {
		if p == "" {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was kept", p)
		}
	}
}

func TestOpenDiskRefusals(t *testing.T) {
	if _, err := OpenDisk("relative/dir", DiskLimits{}); err == nil {
		t.Fatal("a relative directory was accepted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDisk(dir, DiskLimits{}); err == nil {
		t.Fatal("a world-readable directory was accepted")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDisk(file, DiskLimits{}); err == nil {
		t.Fatal("a file was accepted as the directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDisk(dir, DiskLimits{})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
}

// A file planted far over the per-object bound is refused from its size, never
// read: a bomb costs one stat, whatever the memory limit.
func TestDiskOversizedFileIsRefusedWithoutBeingRead(t *testing.T) {
	d, dir := openDisk(t, func(l *DiskLimits) { l.MaxObject = 1024 })
	data := []byte("small")
	id := idOf(t, Blob, data)
	if err := d.Put(id, Blob, data); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, id[:2], id)
	if err := os.Truncate(p, 4<<30); err != nil { // sparse: costs no disk
		t.Skip(err)
	}
	old := debug.SetMemoryLimit(64 << 20)
	defer debug.SetMemoryLimit(old)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, _, ok := d.Get(id); ok {
		t.Fatal("an oversized file was returned")
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Fatalf("a refusal allocated %d bytes", grew)
	}
	if d.Stats().Corrupt != 1 {
		t.Fatalf("%+v", d.Stats())
	}
	// The same file present at open is removed from its size alone.
	if err := d.Put(id, Blob, data); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if err := os.Truncate(p, 4<<30); err != nil {
		t.Skip(err)
	}
	d2, err := OpenDisk(dir, DiskLimits{MaxObject: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.Stats().Entries != 0 {
		t.Fatalf("%+v", d2.Stats())
	}
	if _, err := os.Lstat(p); err == nil {
		t.Fatal("the oversized file was kept at open")
	}
}

func TestDiskIsSafeUnderConcurrentUse(t *testing.T) {
	d, _ := openDisk(t, func(l *DiskLimits) { l.MaxEntries = 20 })
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				data := []byte(fmt.Sprintf("object %d", (g*7+i)%40))
				id, _ := ID(40, Blob, data)
				if err := d.Put(id, Blob, data); err != nil {
					t.Error(err)
					return
				}
				if k, got, ok := d.Get(id); ok && (k != Blob || string(got) != string(data)) {
					t.Errorf("%q %q", k, got)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if s := d.Stats(); s.Entries > 20 || s.Corrupt != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestCacheSecondTierServesWhatTheUpstreamAnsweredOnce(t *testing.T) {
	d, dir := openDisk(t, nil)
	data := []byte("tree-ish\n")
	id := idOf(t, Blob, data)
	calls := 0
	up := func(context.Context) (string, []byte, error) { calls++; return Blob, data, nil }

	c1 := New(Limits{Disk: d})
	if _, got, err := c1.Fetch(context.Background(), id, up); err != nil || string(got) != string(data) || calls != 1 {
		t.Fatalf("%q %v %d", got, err, calls)
	}
	// A fresh process: empty memory, same disk. No upstream call.
	d.Close()
	d2, err := OpenDisk(dir, DefaultDiskLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	c2 := New(Limits{Disk: d2})
	if k, got, err := c2.Fetch(context.Background(), id, up); err != nil || k != Blob || string(got) != string(data) || calls != 1 {
		t.Fatalf("%q %q %v %d", k, got, err, calls)
	}
	if s := c2.Stats(); s.Fetches != 0 {
		t.Fatalf("%+v", s)
	}

	// A damaged file is not trusted: the upstream is asked again, and the disk is repaired.
	if err := os.WriteFile(filepath.Join(dir, id[:2], id), []byte("blob\ntampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	c3 := New(Limits{Disk: d2})
	if _, got, err := c3.Fetch(context.Background(), id, up); err != nil || string(got) != string(data) || calls != 2 {
		t.Fatalf("%q %v %d", got, err, calls)
	}
	if _, _, ok := d2.Get(id); !ok {
		t.Fatal("the disk was not repaired")
	}
}

func TestCacheNeverStoresWhatFailedVerification(t *testing.T) {
	d, _ := openDisk(t, nil)
	id := idOf(t, Blob, []byte("real"))
	c := New(Limits{Disk: d})
	_, _, err := c.Fetch(context.Background(), id, func(context.Context) (string, []byte, error) {
		return Blob, []byte("forged"), nil
	})
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("%v", err)
	}
	if s := d.Stats(); s.Entries != 0 || s.Writes != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestValidIDShapes(t *testing.T) {
	for n, want := range map[int]bool{0: false, 2: false, 39: false, 40: true, 41: false, 63: false, 64: true, 65: false} {
		if got := validID(strings.Repeat("a", n)); got != want {
			t.Errorf("validID of %d hex digits = %v", n, got)
		}
	}
	if validID(strings.Repeat("A", 40)) || validID(strings.Repeat("g", 40)) {
		t.Error("an id that is not lowercase hex was accepted")
	}
}

func TestDiskPutOfAHeldIdChangesNothing(t *testing.T) {
	d, _ := openDisk(t, nil)
	data := []byte("same\n")
	id := idOf(t, Blob, data)
	writes := 0
	d.afterWrite = func() { writes++ }
	for i := 0; i < 3; i++ {
		if err := d.Put(id, Blob, data); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Fatalf("the file was written %d times", writes)
	}
	s := d.Stats()
	if s.Entries != 1 || s.Writes != 1 || s.Bytes != int64(len(Blob)+1+len(data)) {
		t.Fatalf("%+v", s)
	}
}

func TestDiskConcurrentPutOfOneIdIsIndexedOnce(t *testing.T) {
	d, _ := openDisk(t, nil)
	data := []byte("raced\n")
	id := idOf(t, Blob, data)
	d.afterWrite = func() {
		d.afterWrite = nil
		if err := d.Put(id, Blob, data); err != nil { // the other Put wins the index
			t.Error(err)
		}
	}
	if err := d.Put(id, Blob, data); err != nil {
		t.Fatal(err)
	}
	s := d.Stats()
	if s.Entries != 1 || s.Writes != 1 || s.Bytes != int64(len(Blob)+1+len(data)) {
		t.Fatalf("%+v", s)
	}
}

func TestDiskWriteFailuresAreCountedAndLeaveNothing(t *testing.T) {
	d, dir := openDisk(t, nil)
	data := []byte("blocked\n")
	id := idOf(t, Blob, data)
	// The object's directory is a regular file: the temporary file cannot be made.
	if err := os.WriteFile(filepath.Join(dir, id[:2]), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(id, Blob, data); err == nil {
		t.Fatal("a write into a file was accepted")
	}
	if err := os.Remove(filepath.Join(dir, id[:2])); err != nil {
		t.Fatal(err)
	}
	// The destination is a non-empty directory: the rename fails, the temporary
	// file is removed.
	if err := os.MkdirAll(filepath.Join(dir, id[:2], id, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(id, Blob, data); err == nil {
		t.Fatal("a rename onto a directory was accepted")
	}
	ents, err := os.ReadDir(filepath.Join(dir, id[:2]))
	if err != nil || len(ents) != 1 || ents[0].Name() != id {
		t.Fatalf("%v %v", ents, err)
	}
	if s := d.Stats(); s.WriteErrors != 2 || s.Entries != 0 || s.Writes != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestDiskGetOfABadIdIsNotAMiss(t *testing.T) {
	d, _ := openDisk(t, nil)
	if _, _, ok := d.Get("../etc/passwd"); ok {
		t.Fatal("hit")
	}
	if s := d.Stats(); s.Misses != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestDiskGetOfAFileThatIsGoneIsAPlainMiss(t *testing.T) {
	d, dir := openDisk(t, nil)
	data := []byte("vanishing\n")
	id := idOf(t, Blob, data)
	if err := d.Put(id, Blob, data); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, id[:2], id)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := d.Get(id); ok {
		t.Fatal("hit")
	}
	if s := d.Stats(); s.Corrupt != 0 || s.Misses != 1 || s.Entries != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestDiskTruncatedAfterTheStatIsCorrupt(t *testing.T) {
	d, dir := openDisk(t, nil)
	data := []byte("truncated later\n")
	id := idOf(t, Blob, data)
	if err := d.Put(id, Blob, data); err != nil {
		t.Fatal(err)
	}
	// A record shorter than indexed: refused from its size.
	if err := os.Truncate(filepath.Join(dir, id[:2], id), 3); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := d.Get(id); ok {
		t.Fatal("hit")
	}
	if s := d.Stats(); s.Corrupt != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestDiskScanAndOpenFailOnUnreadableDirectories(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory modes do not stop this user")
	}
	d, dir := openDisk(t, nil)
	// The root itself cannot be listed.
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	err := d.scan()
	_ = os.Chmod(dir, 0o700)
	if err == nil {
		t.Fatal("an unreadable root was scanned")
	}
	// A fan-out directory that cannot be listed.
	sub := filepath.Join(dir, "ab")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sub, 0o700)
	if err := d.scan(); err == nil {
		t.Fatal("an unreadable fan-out directory was scanned")
	}
	if _, err := OpenDisk(dir, DefaultDiskLimits()); err == nil {
		t.Fatal("OpenDisk accepted a directory it cannot scan")
	}
}

func TestOpenDiskUnderAFileAndOnAnUnopenableDirectory(t *testing.T) {
	base := t.TempDir()
	f := filepath.Join(base, "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDisk(filepath.Join(f, "cache"), DefaultDiskLimits()); err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("%v", err)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	closed := filepath.Join(base, "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(closed, 0o700)
	if _, err := OpenDisk(closed, DefaultDiskLimits()); err == nil || !strings.Contains(err.Error(), "open") {
		t.Fatalf("%v", err)
	}
}

func FuzzDecodeRecord(f *testing.F) {
	data := []byte("hello\n")
	id, _ := ID(40, Blob, data)
	f.Add(id, encodeRecord(Blob, data))
	f.Add(id, []byte("blob\n"))
	f.Add(id, []byte("\n\n\n"))
	f.Add(id, []byte("commit\nx"))
	f.Add(id[:39], encodeRecord(Blob, data))
	f.Fuzz(func(t *testing.T, id string, raw []byte) {
		kind, got, err := decodeRecord(id, raw)
		if err != nil {
			return
		}
		// Accepted means: a known kind whose bytes hash to the id, nothing else.
		want, werr := ID(len(id), kind, got)
		if werr != nil || want != id {
			t.Fatalf("accepted %q %q for %q", kind, got, id)
		}
		if string(encodeRecord(kind, got)) != string(raw) {
			t.Fatalf("not canonical: %q", raw)
		}
	})
}
