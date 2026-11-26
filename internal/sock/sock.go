// Package sock is the local socket: a directory of mode 0700, a socket of mode
// 0600, and a peer-uid check on every connection (agent-vcs.md, "Local daemon").
package sock

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// peerUID returns the uid of the process on the other end of c. It is a
// per-platform function; platforms without one cannot serve (peer_other.go).
var peerUID func(c *net.UnixConn) (uint32, error)

// Listener accepts only connections whose peer uid equals want.
type Listener struct {
	*net.UnixListener
	want uint32
	// Refused counts connections dropped for a foreign uid.
	Refused func(uid uint32, err error)
}

// Listen creates dir (0700) if needed and a socket in it. It refuses a dir that
// is not owned by the caller or is accessible to others, and never replaces a
// path that is not a socket.
func Listen(path string) (*Listener, error) {
	if peerUID == nil {
		return nil, errors.New("this platform has no peer credential check: the daemon will not serve without one")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		// A socket nobody answers on is stale; one that answers is another daemon.
		if c, err := net.Dial("unix", path); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("%s: another daemon is listening", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// The socket file was created an instant ago in a directory this user alone
	// can enter, so the chmod cannot fail; the mode is a second lock on top of
	// the directory's, and a test asserts it.
	_ = os.Chmod(path, 0o600)
	return &Listener{UnixListener: l, want: uint32(os.Getuid())}, nil
}

// Accept returns the next connection from the daemon's own uid.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		c, err := l.UnixListener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err != nil || uid != l.want {
			_ = c.Close()
			if l.Refused != nil {
				l.Refused(uid, err)
			}
			continue
		}
		return c, nil
	}
}
