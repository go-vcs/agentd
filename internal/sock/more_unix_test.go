//go:build linux || darwin

package sock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForeignOwnedDirIsRefused(t *testing.T) {
	d := filepath.Join(sockDir(t), "d")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkDirOwner(d, os.Getuid()); err != nil {
		t.Errorf("own dir: %v", err)
	}
	if err := checkDirOwner(d, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Errorf("foreign owner: %v", err)
	}
}

func TestListenFailureReasons(t *testing.T) {
	root := sockDir(t)
	f := filepath.Join(root, "file")
	_ = os.WriteFile(f, nil, 0o600)
	if _, err := Listen(filepath.Join(f, "d", "s")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("dir under a file: %v", err)
	}
	// a path longer than sun_path cannot be bound: refused, and not through a later step
	long := filepath.Join(root, strings.Repeat("x", 120), "s")
	if _, err := Listen(long); err == nil || strings.Contains(err.Error(), "chmod") {
		t.Errorf("long path: %v", err)
	}
	// a stale socket in a directory that cannot be written is not silently kept
	if os.Getuid() != 0 {
		d := filepath.Join(root, "ro")
		_ = os.Mkdir(d, 0o700)
		p := filepath.Join(d, "s")
		l, err := Listen(p)
		if err != nil {
			t.Fatal(err)
		}
		l.UnixListener.SetUnlinkOnClose(false)
		l.Close()
		_ = os.Chmod(d, 0o500)
		defer os.Chmod(d, 0o700)
		if _, err := Listen(p); err == nil || !strings.Contains(err.Error(), "remove") {
			t.Errorf("stale socket in a read-only dir: %v", err)
		}
	}
}

func TestAcceptAfterCloseFails(t *testing.T) {
	l, err := Listen(filepath.Join(sockDir(t), "d", "s"))
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if c, err := l.Accept(); err == nil {
		c.Close()
		t.Error("accepted on a closed listener")
	}
}
