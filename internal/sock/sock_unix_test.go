//go:build linux || darwin

package sock

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A socket path must stay under the sun_path limit (104 on macOS), so the
// directory is short; it is still created under the test's own temp area.
func sockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestListenModesAndAccept(t *testing.T) {
	p := filepath.Join(sockDir(t), "d", "s")
	l, err := Listen(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(p)); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	go func() {
		c, err := net.Dial("unix", p)
		if err == nil {
			_, _ = c.Write([]byte("x"))
			c.Close()
		}
	}()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestForeignUIDIsDropped(t *testing.T) {
	p := filepath.Join(sockDir(t), "d", "s")
	l, err := Listen(p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.want = uint32(os.Getuid()) + 1 // pretend the daemon belongs to someone else
	var got uint32 = 1 << 31
	seen := make(chan struct{}, 1)
	l.Refused = func(uid uint32, err error) { got = uid; seen <- struct{}{} }
	go func() {
		if c, err := net.Dial("unix", p); err == nil {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = c.Read(make([]byte, 1)) // returns EOF when the daemon drops us
		}
	}()
	crashed := make(chan any, 1)
	go func() {
		// Closing the listener below makes this Accept return its error; a crash
		// here would kill the binary from a goroutine, so it is carried back.
		defer func() { crashed <- recover() }()
		_, _ = l.Accept()
	}()
	select {
	case <-seen:
	case <-time.After(3 * time.Second):
		t.Fatal("a foreign peer was not refused")
	}
	if got != uint32(os.Getuid()) {
		t.Errorf("refused uid %d", got)
	}
	l.Close()
	select {
	case r := <-crashed:
		reportCrash(t, r)
	case <-time.After(3 * time.Second):
		t.Error("Accept did not return after the listener was closed")
	}
}

func TestListenRefusals(t *testing.T) {
	root := sockDir(t)
	// open directory
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(open, 0o755)
	if _, err := Listen(filepath.Join(open, "s")); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("open dir: %v", err)
	}
	// a regular file in the way is never replaced
	d := filepath.Join(root, "d")
	_ = os.Mkdir(d, 0o700)
	f := filepath.Join(d, "s")
	if err := os.WriteFile(f, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(f); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("file in the way: %v", err)
	}
	if b, _ := os.ReadFile(f); string(b) != "keep" {
		t.Error("the file was replaced")
	}
	_ = os.Remove(f)
	// a live daemon is never displaced; a stale socket is replaced
	l, err := Listen(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(f); err == nil || !strings.Contains(err.Error(), "another daemon") {
		t.Errorf("second daemon: %v", err)
	}
	l.UnixListener.SetUnlinkOnClose(false)
	l.Close()
	l2, err := Listen(f)
	if err != nil {
		t.Errorf("stale socket: %v", err)
	} else {
		l2.Close()
	}
}

func TestNoPeerCheckNoService(t *testing.T) {
	saved := peerUID
	peerUID = nil
	defer func() { peerUID = saved }()
	if _, err := Listen(filepath.Join(sockDir(t), "s")); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("served without a peer check: %v", err)
	}
}
