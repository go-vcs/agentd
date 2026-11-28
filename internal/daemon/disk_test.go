package daemon

import (
	"path/filepath"
	"testing"

	"github.com/go-vcs/agentd/internal/objcache"
	"github.com/go-vcs/agentd/internal/verdict"
	"github.com/go-vcs/agentd/internal/wire"
)

// A restarted daemon with the same disk cache checks a commit out without one
// upstream request: the objects come back from disk, verified again.
func TestRestartedDaemonServesACheckoutFromTheDiskCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "objects")
	open := func() *objcache.Disk {
		d, err := objcache.OpenDisk(dir, objcache.DiskLimits{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		return d
	}
	d1 := open()
	be1, commit, _, _, do1 := mat(t, t.TempDir(), func(l *Limits) { l.Cache.Disk = d1 })
	if code, body := do1(wire.MaterializeReq{Commit: commit, Dir: "a"}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if n := be1.fetches.Load(); n != 5 {
		t.Fatalf("%d upstream fetches the first time", n)
	}
	d1.Close()

	be2 := &objBE{fakeBE: fakeBE{state: verdict.Green}, objects: be1.objects}
	root := t.TempDir()
	d2 := open()
	ts, _ := start(t, be2, func(l *Limits) { l.MaterializeRoot = root; l.Cache.Disk = d2 })
	sess := hello(t, ts, "b")
	code, body := post(t, ts, "/v1/materialize", wire.MaterializeReq{Session: sess, Repo: "o/r", Commit: commit, Dir: "b"})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if n := be2.fetches.Load(); n != 0 {
		t.Fatalf("%d upstream fetches after a restart with a warm disk", n)
	}
	if s := d2.Stats(); s.Hits != 5 {
		t.Fatalf("%+v", s)
	}
}
