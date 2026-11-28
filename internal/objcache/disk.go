package objcache

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// DiskLimits bound a disk cache.
type DiskLimits struct {
	MaxBytes   int64 // file bytes of everything held
	MaxEntries int
	MaxObject  int64 // largest single object admitted or read back
}

// DefaultDiskLimits: 10 GiB (the design's default), 1000000 objects, 2 MiB per
// object.
func DefaultDiskLimits() DiskLimits {
	return DiskLimits{MaxBytes: 10 << 30, MaxEntries: 1000000, MaxObject: 2 << 20}
}

// DiskStats are exact counters.
type DiskStats struct {
	Entries                       int
	Bytes                         int64
	Hits, Misses, Writes, Evicted int64
	Corrupt                       int64 // files found damaged on read and removed
	Refused                       int64 // objects refused by Put
	WriteErrors                   int64 // writes the filesystem failed
}

// maxKind is the longest kind a record names ("commit").
const maxKind = len(Commit)

type dent struct {
	id   string
	size int64
}

// Disk is a bounded object store under one private directory. Every object is
// recomputed against its id when written AND when read back: a file that was
// changed, truncated, swapped or planted is a miss, and is removed. Safe for
// concurrent use; one process owns a directory.
type Disk struct {
	root *os.Root
	lim  DiskLimits

	mu    sync.Mutex
	ll    *list.List // front = most recently used
	m     map[string]*list.Element
	bytes int64
	st    DiskStats

	afterWrite func() // test hook: runs between a Put's write and its index update
}

// OpenDisk opens (creating it with mode 0700) the cache directory dir, which
// must be an absolute path. It refuses a directory that is group- or
// world-accessible, indexes what it holds (dropping stray files, and evicting
// the oldest until the bounds hold), and removes leftovers of an interrupted
// write. The caller chooses a durable location: this is never a temporary one.
func OpenDisk(dir string, l DiskLimits) (*Disk, error) {
	d := DefaultDiskLimits()
	if l.MaxBytes <= 0 {
		l.MaxBytes = d.MaxBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = d.MaxEntries
	}
	if l.MaxObject <= 0 {
		l.MaxObject = d.MaxObject
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("disk cache directory %q must be an absolute path", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := root.Stat("."); err != nil || !fi.IsDir() || runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		root.Close()
		return nil, fmt.Errorf("disk cache directory %s must be a directory with mode 0700", dir)
	}
	c := &Disk{root: root, lim: l, ll: list.New(), m: map[string]*list.Element{}}
	if err := c.scan(); err != nil {
		root.Close()
		return nil, err
	}
	return c, nil
}

// Close releases the directory.
func (c *Disk) Close() error { return c.root.Close() }

// validID reports whether id is a full lowercase hex object id: the only shape
// ever turned into a path.
func validID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

func relPath(id string) string { return id[:2] + "/" + id }

type found struct {
	id   string
	size int64
	mod  int64
}

// scan indexes the directory: oldest first at the back of the list.
func (c *Disk) scan() error {
	top, err := listDir(c.root, ".")
	if err != nil {
		return err
	}
	var all []found
	for _, e := range top {
		name := e.Name()
		if !e.IsDir() || len(name) != 2 || !validID(name+strings.Repeat("0", 38)) {
			_ = c.root.RemoveAll(name) // a stray: not ours
			continue
		}
		files, err := listDir(c.root, name)
		if err != nil {
			return err
		}
		for _, f := range files {
			p := name + "/" + f.Name()
			fi, err := c.root.Lstat(p)
			if err != nil || !fi.Mode().IsRegular() || !validID(f.Name()) || f.Name()[:2] != name ||
				fi.Size() > c.lim.MaxObject+int64(maxKind)+1 {
				_ = c.root.RemoveAll(p) // a leftover temp file, a link, a stray, an oversized file
				continue
			}
			all = append(all, found{f.Name(), fi.Size(), fi.ModTime().UnixNano()})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].mod != all[j].mod {
			return all[i].mod > all[j].mod // newest first: pushed to the back last
		}
		return all[i].id < all[j].id
	})
	for _, f := range all {
		c.m[f.id] = c.ll.PushBack(&dent{f.id, f.size})
		c.bytes += f.size
	}
	c.mu.Lock()
	c.evict(0, 0)
	c.mu.Unlock()
	return nil
}

// listDir lists a directory of the root.
func listDir(r *os.Root, name string) ([]os.DirEntry, error) {
	return fs.ReadDir(r.FS(), name)
}

// evict removes least recently used files until n more bytes and extra more
// entries fit. Called with mu held.
func (c *Disk) evict(n int64, extra int) {
	for c.ll.Len() > 0 && (c.bytes+n > c.lim.MaxBytes || c.ll.Len()+extra > c.lim.MaxEntries) {
		back := c.ll.Back()
		e := c.ll.Remove(back).(*dent)
		delete(c.m, e.id)
		c.bytes -= e.size
		_ = c.root.Remove(relPath(e.id))
		c.st.Evicted++
	}
}

// errRecord marks a record that is not the object it claims to be.
var errRecord = errors.New("disk record is not a verified object")

// encodeRecord is the file content: the kind, a newline, the object bytes.
func encodeRecord(kind string, data []byte) []byte {
	b := make([]byte, 0, len(kind)+1+len(data))
	b = append(b, kind...)
	b = append(b, '\n')
	return append(b, data...)
}

// decodeRecord parses a file's content and verifies it against id: the kind
// must be one of ours and the bytes must hash to id under it.
func decodeRecord(id string, raw []byte) (kind string, data []byte, err error) {
	lim := len(raw)
	if lim > maxKind+1 {
		lim = maxKind + 1
	}
	nl := -1
	for i := 0; i < lim; i++ {
		if raw[i] == '\n' {
			nl = i
			break
		}
	}
	if nl < 0 {
		return "", nil, errRecord
	}
	kind, data = string(raw[:nl]), raw[nl+1:]
	got, err := ID(len(id), kind, data)
	if err != nil || got != id {
		return "", nil, errRecord
	}
	return kind, data, nil
}

// Put stores the object after recomputing its id; a mismatch, an object over
// the per-object bound and an id that is not a full hex id are refused. It
// evicts the least recently used objects to make room.
func (c *Disk) Put(id, kind string, data []byte) error {
	if !validID(id) {
		c.refuse()
		return ErrID
	}
	if int64(len(data)) > c.lim.MaxObject || int64(len(data)) >= c.lim.MaxBytes {
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
	if el, ok := c.m[id]; ok {
		c.ll.MoveToFront(el)
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	rec := encodeRecord(kind, data)
	if err := c.write(id, rec); err != nil {
		c.mu.Lock()
		c.st.WriteErrors++
		c.mu.Unlock()
		return err
	}
	if c.afterWrite != nil {
		c.afterWrite()
	}
	n := int64(len(rec))
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[id]; ok { // a concurrent Put of the same id won: same bytes
		c.ll.MoveToFront(el)
		return nil
	}
	c.evict(n, 1)
	c.m[id] = c.ll.PushFront(&dent{id, n})
	c.bytes += n
	c.st.Writes++
	return nil
}

// write puts rec at the id's path through a temporary file and a rename, mode
// 0600, so a reader sees all of it or none of it.
func (c *Disk) write(id string, rec []byte) error {
	sub := id[:2]
	// An existing directory is the normal case; any other failure is reported by
	// the OpenFile below, which cannot succeed without the directory.
	_ = c.root.Mkdir(sub, 0o700)
	var rnd [8]byte
	_, _ = rand.Read(rnd[:]) // crypto/rand.Read does not return an error
	tmp := sub + "/.tmp-" + hex.EncodeToString(rnd[:])
	f, err := c.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, err = f.Write(rec)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = c.root.Rename(tmp, relPath(id))
	}
	if err != nil {
		_ = c.root.Remove(tmp)
		return err
	}
	return nil
}

func (c *Disk) refuse() {
	c.mu.Lock()
	c.st.Refused++
	c.mu.Unlock()
}

// Get returns the object under id when the disk holds it and the bytes still
// hash to id. A file that does not is removed and reported as a miss. The size
// is checked against the bound before anything is read.
func (c *Disk) Get(id string) (kind string, data []byte, ok bool) {
	if !validID(id) {
		return "", nil, false
	}
	c.mu.Lock()
	el, held := c.m[id]
	if !held {
		c.st.Misses++
		c.mu.Unlock()
		return "", nil, false
	}
	size := el.Value.(*dent).size
	c.mu.Unlock()

	raw, err := c.read(id, size)
	if err == nil {
		kind, data, err = decodeRecord(id, raw)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// Act on the entry only if it is still the one that was read: a concurrent
		// eviction, or an eviction and a new Put, must not be undone or blamed here.
		if cur, ok := c.m[id]; ok && cur == el {
			c.ll.Remove(el)
			delete(c.m, id)
			c.bytes -= size
			_ = c.root.Remove(relPath(id))
			if !errors.Is(err, os.ErrNotExist) {
				c.st.Corrupt++ // a file that is gone was not damaged
			}
		}
		c.st.Misses++
		return "", nil, false
	}
	if cur, ok := c.m[id]; ok && cur == el {
		c.ll.MoveToFront(el)
	}
	c.st.Hits++
	return kind, data, true
}

// read returns the file's bytes when it is a regular file of exactly the
// indexed size and within the per-object bound.
func (c *Disk) read(id string, size int64) ([]byte, error) {
	f, err := c.root.Open(relPath(id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() != size || size > c.lim.MaxObject+int64(maxKind)+1 {
		return nil, errRecord
	}
	// A file that shrinks after the Stat reads short: the bytes then fail the
	// hash in decodeRecord, which is where every damaged file is refused.
	raw := make([]byte, size)
	n, _ := io.ReadFull(f, raw)
	return raw[:n], nil
}

// Stats returns the counters.
func (c *Disk) Stats() DiskStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st
	s.Entries, s.Bytes = c.ll.Len(), c.bytes
	return s
}
